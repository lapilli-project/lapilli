# Changelog

## [Unreleased]

### Changed

- **`lapilli serve` says at info, once, that it didn't ask the history's rules API** when `--max-rate`'s turn wouldn't come within the 30 seconds that read has, where it warned that the API couldn't be read; the groups are placed as before.

## [0.72.0] - 2026-11-27

What people did when a rule fired (pages acknowledged or not, changes on
the alert's target, rules found noisy by them) and who a firing reached,
worked out from the alert manager's routing and held to what it really
sent. An alert on a dependency catches its dependents' incidents, by the
service graph the ledger keeps. A pull request's backtest through serve
replays the rules it leaves alone where the record can't settle them, and
compares Loki, Grafana and cloud rules as parsed. Serve judges a firing by
its day's record. The names made one thing before 1.0, each old one still
taken. `--check-replay` says which firings differ, a change on the week
before reads one way, and who is signed in and the language are menus at
the top right. And what merged since 0.71.0: Terraform's
`grafana_rule_group` read as rules, `--firings` beside the rules replayed,
a catch without the label said, and the replay checked against what a real
Prometheus fired on kube-prometheus-stack's default rules.

Before upgrading:
- **Ledger schemas 032 to 036**, after 031, in one start: keep a copy of
  the ledger's disk, or of its database, first
  ([Going back after a schema upgrade](charts/lapilli/README.md#going-back-after-a-schema-upgrade)).
  A lapilli before this one doesn't open the upgraded ledger.
- **Serve judges a firing by its day's record, not its week's**: a firing
  on a day before an incident started in its week is no longer judged, and
  one after an incident's two weeks ended in its week is judged no more.
  Nothing is judged again. `--record-by-week` keeps the old.
- **A flag, route or chart value renamed** is still taken under its old
  name until the next major release; stderr or the chart's NOTES says so.
  A key set under both names with different values is an error naming
  both.
- **`helm upgrade --reuse-values` from 0.71.0** keeps its `image.digest`,
  which now fails: give `image.digest` this chart's digest, or `""`.

### Added

- **`lapilli verdict --check-replay` says which firings differ**
  ([docs/check-replay.md](docs/check-replay.md)): of each rule that does, the first five
  only in the replay and the first five only in what really fired, from when to when, with
  their labels, and `--json` the first twenty of each, as a rule's `only_replayed_firings`
  and `only_real_firings` (added; nothing renamed).

- **A flag, a route or a chart value renamed is still taken under its old name**
  ([docs/upgrading.md](docs/upgrading.md#renamed-names)), the mechanism that
  ROADMAP's "Toward 1.0" promises. A flag by its old name is read as
  the new one and stderr says `lapilli <command>: --old is --new now; --old will go in a major
  release` once; a flag given under both names is an error. A request to a route's old path is
  answered by the new route's handler, with its sign-in, roles and limits, and carries
  `Deprecation: @<seconds>` (RFC 9745) and `Link: <new path>; rel="successor-version"`. A chart
  value under its old key is read as the new, and `NOTES.txt` says so. The tables are
  `renamedFlags` (`cmd/lapilli/renamed.go`), `renamedRoutes` (`internal/serve/renamed.go`) and
  `templates/_renamed.tpl`.

- **An alert on a dependency catches its dependents' incidents**
  ([docs/dependencies.md](docs/dependencies.md),
  [docs/design/dependencies.md](docs/design/dependencies.md)). `--depends PROMQL`, with
  `--depends-labels FROM,TO`, reads who calls whom (a service mesh's or an OpenTelemetry
  service graph's calls by caller and callee, counted over the hour, `[1h]`) once, at
  `--depends-step` (1h, 10m to 1h; a sample is stored as the hour it covers), and a firing whose `--match` label isn't the incident's catches it by dependency
  when the incident's service calls the firing's, or one that does, up to `--depends-depth`
  calls (1, at most 3) over the edges of the hours from an hour before the incident to its
  end, and it agrees on every other `--match` label both carry; `--depends-direction both`
  also lets a caller's alert catch what it calls, and `--depends-match` names the label whose
  values are the services (the first `--match` label by default). Linked, then labels, then
  dependency: a catch by dependency is a catch, marked `(via checkout → payments-db)`
  (`dependency` on `caught_by[]` in `--json`, in `--compare`'s text and Markdown, where each service is a code span as a rule's name is, aliased by
  `--redact`), near misses (with the path, `dependency`) and "after its end" follow it, and the report says after the
  incidents table how many catches were so, by which rules, and how many edges the graph had
  (`settings.depends`), with how many firings it made white that nothing else caught
  (`white_by_dependency` on a rule, `by_dependency` on a firing); a rule with a direct firing
  has its delay and looseness from the direct ones alone. The graph keeps no record and never takes a catch away; an hour with
  more than 20,000 edges leaves it with none, which the report says, and `--compare` too. `lapilli impact suggest`
  says which of Istio, Linkerd and an OpenTelemetry or Tempo service graph have their calls
  in the history, and the `--depends` and `--depends-labels` to give. With no `--depends`
  every report is as it was.
- **A catch by dependency, held to what anyone could write.** The graph says who calls whom, and
  whoever can write that series can make a call, so: a catch made only by dependency no longer
  counts as a catch for `--fail-missed` (a change that removes an incident's only alert by its labels
  is missed and fails the run, though another rule caught it by the graph; `missed_by_dependency` in
  `--compare --json`, a subset of `missed`, and a line in its text and Markdown say how many and why) nor covers another rule for "spare" or takes its `FIRST` and
  `ALONE`; `--depends-count-catches` counts them as any catch (`settings.depends.count_catches`). The
  walk is made once for an incident, to every service it reaches, and each firing is a lookup
  (`graph.Reach`; the cost is the incidents, not the firings times the incidents; `Depends` is a
  lookup in it). A service name that isn't printable, holds a format or bidirectional-control
  character, a space or an arrow of any kind, or `,`, or is longer than 317 bytes (a Kubernetes `namespace/name`; was 256) is refused and counted
  (`settings.depends.refused`), so a path's text can't be forged, and an hour that names more than
  5,000 services leaves the graph with none, as 20,000 edges do; `verdict.DependsHops` writes a path
  as separate escaped hops, each in its own `<bdi>`, for a page. An hour of the ledger's rows that
  is too many (the union of overlapping reads) drops that hour alone and says so, not the whole
  judgement's graph; the walk's cache is bounded (about 100 MiB with a judgement's edges), and the
  adjacency of a window is kept, so incidents sharing one don't build it again. `lapilli impact suggest`'s Istio
  and Linkerd expressions name a service `namespace/workload` (labels `caller,callee`), so
  same-named workloads of two namespaces don't merge and another namespace's can't be named as the
  one a service calls: the firings' and incidents' `--match` label must carry that form
  ([docs/dependencies.md](docs/dependencies.md) shows how, and lists the plain form as an option);
  an OpenTelemetry service graph is named by its senders, and the docs say so.
- **An alert on a dependency, in `lapilli serve`**
  ([docs/dependencies.md](docs/dependencies.md#in-lapilli-serve),
  [docs/design/dependencies.md](docs/design/dependencies.md)). `lapilli serve` takes
  `--depends` and the flags that go with it (`--depends-labels`, `--depends-match`,
  `--depends-depth`, `--depends-direction`, `--depends-step`; the chart's `depends`,
  `dependsLabels`, `dependsMatch`, `dependsDepth`, `dependsDirection` and `dependsStep`, in
  both modes). Each run reads the graph for the hours it judges and the lead before them,
  once, and keeps its edges by the hour (schema 036: `edges`, `edge_spans`, `edge_retries`,
  `catches.dependency`) for as long as the firings; a read that fails is read once more by
  the next run, as the changes `--rollouts` reads are. Judging, the first time and again, asks
  the history nothing: it loads the ledger's edges for the hours of the incidents it judges
  against, 1,000,000 at most in one judgement (past it that judgement is made with no graph).
  Turning `--depends` on, off, or changing its expression, labels, step, label, depth or
  direction judges every firing again, once. The Settings page's Incident criteria has a
  **Service graph** with the same four settings, tried on the last four weeks first like
  `--impact` (the catches it would add, by rule, and how the verdict moves), by an admin, from
  the site, counted with the impact tries; saved, the graph is read over those weeks and every
  firing judged again. The incident's page shows a catch by dependency with its path, a rule's
  page lists its catches by dependency apart, and MCP and `/api/v1/incidents` carry
  `dependency` (and `/api/v1/report` its `settings.depends`). Without `--depends` nothing is
  read, kept or shown, and the stones are as they were. The ledger judges as `lapilli
  verdict` does: a rule with a direct firing has its delay and looseness from the direct ones
  alone, and the firings white only by the graph are counted (`white_by_dependency` on the
  report's rules, the digest's note, the rule page, a mark beside the overview's precision, MCP);
  a judgement loads each incident's window apart, not the hours between them, and one whose
  windows pass 1,000,000 edges is made with no graph and says so (`too_many`, the Settings and
  rule pages, the log). A round reads and keeps only its own graph; a new expression is read
  over the last four weeks a week at a time, within 90 seconds each, tried again an hour later,
  then two, up to a day when none reads, with the weeks that read kept and the others read once
  more, then shown as gaps, and why it failed kept in the ledger for every replica; the key
  is the expression as Lapilli reads PromQL, so spaces don't discard the edges. The ledger keeps
  200,000 edges a day and 5,000,000 in all, past which the hours are not read, with a status.
- **A pull request backtest through serve replays the rules left alone that
  its record can't settle** ([docs/design/backtests.md](docs/design/backtests.md#held-to-the-incidents-step-4)).
  With `fail_missed`, an incident the rules that differ lost that serve's
  record leaves not known because of the rules left alone (a rule serve
  doesn't run, a version over the time that is another rule, runs that
  didn't cover it, a group's `query_offset` its rules API doesn't say) is
  settled by replaying them, in the backtest's lane, over that incident's
  catch window and each rule's reach before it: caught, or missed, which
  fails the job. A rule whose record is held isn't replayed, and when the
  record settles every incident nothing is. The replay is held to what the
  lane's share of the rate asks within what is left of
  `--backtest-timeout`, and to
  `--max-samples`; the incidents it doesn't reach, or whose rule couldn't be
  replayed, stay not known, and the summary of `--compare` says how many
  were left by the limit, `missed_limited` (0 when none were; the summary line of the comment and
  `--compare`'s `summary` gain it, at the end), and the
  comment says so. A rule left alone that serve doesn't run no longer
  leaves an incident not known on that account alone.
- **A Grafana rule compared as parsed**, in `lapilli serve`'s pull request
  backtests (0.72). A rule as a pull request's provisioning file writes it
  and the same rule as Grafana's export gives it, which fills in fields on
  save (`intervalMs`, `maxDataPoints`, `refId`, `range`, `isPaused`, a
  model's `datasource`), are one rule when what decides when and how it
  fires is the same (queries, thresholds, math, reduces, classic
  conditions, the condition, `for`, `keepFiringFor`, the labels, the
  no-data and error states), so a rule the change left alone is held to
  serve's record and its catches are known, where before the two never
  matched. An annotation, notification settings, ids and
  provenance don't make another rule. A reduce is read as Grafana 13.3
  reads it (its replacement is `replaceWithValue`; a mode Grafana refuses,
  `strict` among them, is no rule it loads), a query's `intervalFactor` is
  held, and a math expression its lexer refuses (`$A>-1`, `$A--$B`, `^`)
  is no rule either. A rule with no graph, or one that
  can't be read so, is still compared by its text.
- **A Loki ruler's rules and a cloud's PromQL alarms compared as parsed**, as
  Prometheus's are ([docs/design/backtests.md](docs/design/backtests.md)).
  A pull request may write a rule otherwise than serve's copy of it from
  the ruler's configuration API (spacing, quotes, the order of matchers),
  so the two were never one by their text, and serve held the catch of a
  pull request's rule left alone as not known. A rule's namespace (where a
  ruler keeps its group, the source of an alarm) is no part of what makes
  two rules one, as a rule file has none. A rule's
  LogQL is now read and written in one form (a binary operation between
  parentheses, a stream selector's matchers sorted, strings, numbers and
  durations by value, `by` before or after) for the forms Loki 3.7.9 reads
  alike, and what it may read otherwise (`on`, `variants`, `ip`, a line
  filter's `or`, several label filters in one stage) is compared by its
  text as before, and so is what Loki 3.7.9 refuses (`sum(x) > -0`,
  `offset 0`, `rate(...) by (a)`, `sort by (a)`, a selector whose matchers
  all match an empty value, a regular expression it can't compile), so an
  expression the ruler wouldn't load is never one with one it would; the
  rules of the recorded Loki and its API's answer are
  one each. A CloudWatch, Google Cloud or Azure alarm in PromQL is read
  as PromQL, with its cloud in the result: never one with another cloud's
  or Prometheus's rule.
- **Who a firing reached, in `lapilli serve`**
  ([docs/notifications-worked-out.md](docs/notifications-worked-out.md#in-lapilli-serve),
  [docs/design/routing.md](docs/design/routing.md)). Given `--alertmanager-config` or
  `--grafana`, each run works out the notifications of the hours it judges, with the
  longest `repeat_interval` of lead-in, from the firings in the ledger (so judging again
  works them out again without asking the history), and keeps them for each firing and
  each notification (schema 035): the receivers and integration kinds it reached, its
  notifications and pages, when the first page and each went, and what muted it.
  Silences are `--silences` (read again each run) and the mutes the event recorder told of.
  Where notifications were really recorded, the recorded ones count, and the worked-out
  ones are held to them: the new **Notifications** page lists, for each receiver, those
  worked out and those recorded, and `lapilli_notifications_agreement` is, over the
  receivers with both, the sum of the smaller of the two counts over the sum of the larger.
  Where none were recorded, the worked-out ones count, and every page and API that shows
  pages says "worked out", "recorded" or both: the rule page, the rules list, the overview's
  "Woken for nothing" (pages, not every notification), `/api/v1/summary` (`paged`), goals
  (`pages`, `pages.asleep`, `pages.nothing`), a budget of `N pages` (which
  `lapilli serve` took as "not known" before), the weekly digest, MCP's rule and firing
  tools, `/metrics` (`lapilli_rule_pages`) and `/api/v1/report` in the fields `lapilli
  verdict` gives. The flags are `--notifications-worked-out=false`, `--external-labels`, `--silences`
  and `--paging-receivers`, which `lapilli serve` refused before; the chart's
  `notifications` values (`workedOut`, `externalLabels`, `pagingReceivers`, `silences`) pass
  them in both modes. Schema 035 only adds tables. A work-out is bounded (2 minutes, 20,000,000
  work units, 300,000 firings with the run's own hours first, 100,000 silences), takes a firing that
  began before the lead-in as notified before it (no first page every hour; its repeats counted from
  the last notification kept for its group), counts what a firing reached, when it is read, from the notifications kept for it in the hours worked out (so one hour not worked out, or not tried yet, in a firing's life, which no longer moves the phase of a group's repeats for the hours after it, makes that hour's week not known, and not the pages of a long firing's other weeks; a page is known for a receiver as recorded where it has recordings in the hour, otherwise as worked out where the hour is covered, and not otherwise, whatever another receiver recorded; the cover and the rows are read together, a view reads at most 500,000 pages and says "not known" past them, a catch-up piece that used all of its own 2 minutes backs off, and marking hours not worked out drops what was said of the firings in them), and records the hours of a run that stopped, failed, couldn't read the
  configuration or whose firings were recorded again as not worked out (`routed_cover`),
  which every page says as "not known" (a week touching one, `paged.unknown` in
  `/api/v1/summary`, a budget of pages, a goal, the digest) and the catch-up works out
  again; the catch-up takes 2 minutes in all and a piece that stopped waits an hour, then two, doubling to a day. A recorded page is a page by the model's rule: to PagerDuty, Opsgenie, VictorOps or incident.io only for a
  firing notification after none or after all resolved, unless `--paging-receivers` matches the receiver; hours recorded before the first one worked out are known as recorded; a page worked out just before an hour and its recorded copy just after count once; recorded or worked out is decided for
  each receiver and hour; a silence is tied to the firing it muted; an inhibition's
  `source_rule` is kept; a receiver recorded by name alone is compared with its webhooks, and
  one with only recorded notifications is shown apart; replicas read the configuration for their
  pages; pruning takes the notifications worked out and their cover.
- **A Grafana rule that names its own contact point** (`notification_settings`) is routed
  there, past the notification policies, with its own `group_by`, `group_wait`,
  `group_interval`, `repeat_interval` and mute and active timings, in `lapilli verdict`
  and `lapilli serve`, as Grafana does; before, it was routed by the policies like the
  others, so its pages could go to the wrong receiver.
- **Who a firing reached, worked out**, in `lapilli verdict`
  ([docs/notifications-worked-out.md](docs/notifications-worked-out.md),
  [docs/design/routing.md](docs/design/routing.md)). Given the alert manager's
  configuration (`--alertmanager-config`, or `--grafana`, whose notification
  policies, contact points and mute timings are read), Lapilli plays the
  firings of the period through the routes, the groups, the inhibition rules,
  the silences and the time intervals as Alertmanager's dispatcher does, and
  says for each firing which receivers it reached, in which notifications,
  which of them page someone, and what muted it; and for each rule NOTIFIED,
  PAGED, the pages while people slept and out of hours, **pages for nothing**
  (of firings that caught nothing, or, with `--responses`, weren't useful),
  how often an inhibition rule muted it and the rule that did most, and how
  many firings went out grouped with other rules' alerts. `--external-labels
  NAME=VALUE` (repeatable) are the labels Prometheus adds to each alert,
  `--silences FILE` the silences Alertmanager held, `--paging-receivers REGEX`
  the receivers that page whatever they send with (otherwise pagerduty,
  opsgenie, victorops and pushover, and Grafana's oncall, page, and the rest are
  messages), and `--notifications-worked-out=false` leaves it all out. The text has a
  NOTIFICATIONS table, and its out-of-hours section counts pages for nothing;
  `--json` has `notifications` on the report, each rule and each firing,
  `--csv` has `worked_out_*` columns after the others, `--metrics` has
  `lapilli_rule_notifications_worked_out` and `lapilli_rule_pages_worked_out`, and
  `--redact` gives receivers, inhibition rules, silences and time intervals an
  alias. `--compare` and a pull request's backtest start with the pages before and
  after, `Pages 12 → 15`, and show PAGED for each rule that differs; the summary
  has `notifications` when they were worked out. An expectation's budget takes
  `pages` beside `week`, `asleep` and `refires` (`lapilli verdict --compare`; not
  known in `lapilli serve` yet). The numbers are worked out from the
  configuration, not recorded, and every surface says so. With no
  configuration, or `--notifications-worked-out=false`, every output is what it was. Not in
  `lapilli serve` yet.
- **What people did when a rule fired**, in `lapilli verdict`
  ([docs/responses.md](docs/responses.md)). `--responses FILE` reads the pages
  a paging tool sent and whether a person acknowledged each; `--rollouts`
  reads rollouts, rollbacks and scales of the alert's workload from
  kube-state-metrics in the same history (`--rollout-match LABEL` ties a
  firing to a workload by a label); `--deploys FILE` reads the deploys file
  `lapilli serve` takes. Any of them gives each firing a response (changed,
  acknowledged, unanswered, routine or none) and says whether it was useful,
  and each rule an actionable share beside its precision. `--routine P`
  (0.5) sets when a change is chance. `--noisy-by caught|useful` says what a
  rule is found noisy by, `useful` once `--responses` or `--rollouts` is given
  (`--deploys` alone leaves it `caught`, as serve, which took `--deploys`
  before, must not change which rules are noisy on an upgrade), so a rule
  whose pages nobody answered is noisy though no incident was written down;
  the longer `for` suggested then keeps every useful firing. The text has a
  RESPONSES table; `--json` has `response` on each firing, `responses` on each
  rule and the report, and `settings.noisy_by` and `settings.routine`;
  `--csv` has columns after the others, `--metrics` has
  `lapilli_rule_responses` and `lapilli_rule_actionable`, `--compare` shows
  ACTIONABLE beside precision, and `--redact` gives page ids an alias. With
  none of the three, every output is what it was. No person's name is read.
- **`lapilli serve` reads what people did when a rule fired** (schema 034,
  [docs/design/responses.md](docs/design/responses.md)). `--responses FILE` is
  read again each run, and `--responses-token-file SOURCE=FILE` (once for
  each source: a token writes its source's pages only, and the file's pages
  outrank an API source's) takes pages on the incidents port, `PUT
  /api/v1/responses/SOURCE?from=&to=`, JSON of 16 MiB at most, replacing that
  source's pages sent in the period (31 days at most, none sent more than an
  hour ahead, 50 sources, 500,000 pages a source and 200,000 a week, read by
  their firings' rules). A change new to the ledger judges its target's
  firings of that week again, and a deploy that starts after a firing did
  judges that firing again; a read of the changes that failed is read once
  more by the next run. Every page that says a rule is noisy asks one test,
  and a longer `for` keeps the useful firings serve's digest is given
  judged. A paging recipe says counts, not ids, on stderr. `--rollouts` reads the changes made to
  the workloads of the firings' namespaces with each run and keeps them, as
  `--deploys` does for deploys, so that judging the ledger again asks the
  history nothing; `--rollout-match`, `--routine` and `--noisy-by` are as in
  `lapilli verdict`, and the Settings page keeps `noisy-by` (`useful` by
  default once `--responses`, `--responses-token-file` or `--rollouts` is given). Each
  firing's response and usefulness are worked out as its stones are, a
  person's answer first, and counted by day and week; the report, the
  summary rows (`responses`, `actionable`), the goals (`actionable.share`),
  `/metrics` (`lapilli_rule_responses`, `lapilli_rule_actionable`), the
  weekly digest and MCP's rule and firing tools say them. The first start
  judges every firing again (`judgedBy` j3). The chart has `rollouts`,
  `rolloutMatch`, `routine`, `noisyBy`, `serve.responses` (a token for each source,
  `tokenSecrets`, and the file) and runs the `responses/` recipes beside `recipes/incidents`.
- **The web UI says what was done about a firing**
  ([docs/serve.md](docs/serve.md#what-was-done-about-a-firing)), once serve
  keeps pages or changes read from the history, or rules are found noisy by
  `useful`: a Response column on Alert history and on a rule's latest firings
  ("Rolled out 12:04", "Acknowledged", "Nobody acknowledged; resolved by its
  alert manager"), picked by `?response=changed|acknowledged|unanswered|
  routine|none`; Actionable beside Precision on a rule's page, as a column
  and an order on the rules list; "Pages nobody answered" on the overview,
  last week beside the week before; and `noisy_by` on the Settings page as a
  choice, tried on the last week first as the noisy thresholds are. In
  English, Korean, Japanese and Spanish. Nothing changes where responses
  don't show.
- **`--noisy-by` is `useful` by default only with `--responses` or
  `--rollouts`**, in both commands: `--deploys` alone leaves `caught`, since
  `lapilli serve` took `--deploys` before it took responses and an upgrade
  must not change which rules are noisy. `--noisy-by` given always wins.
- **The backtest's report in the step's summary**: in GitHub Actions the
  recipe adds it to `GITHUB_STEP_SUMMARY`, shown on the run's page, the
  comment's Markdown without its hidden first line. A merge queue's run,
  which posts no comment, has it there as well as in its log; a pull
  request's run has it whether or not a comment was posted. Nothing is
  added past the 1 MiB GitHub takes of a step's summary, or to a summary
  that is a link, and the job's log says so.
- `lapilli verdict --firings FILE` goes beside rules replayed (`--rules`,
  `--loaded-rules`, `--grafana`, `--ruler`, `--alarms`, `--loki-rules`,
  `--rules-history`): what a cloud's own alarms fired, as recipes/firings
  records it, is judged with the replayed rules in one report, so an
  incident only an alarm caught is no longer missed in the rules' report.
  Its rules are said as recorded, not replayed: no longer "for", near
  misses or no-data check for them. A name both replayed and recorded is
  refused; `--firings-prefix STR` puts STR before the recorded ones' names.
  `--fail-nothing-replayed` and `--fail-unreplayed` count only rules
  replayed. Not with `--alerts` or `--compare`.
- Rules from Terraform: the JSON `terraform show -json` writes of a plan
  or a state is read wherever rules files are (`--rules`, serve's rules,
  preflight, a pull request's backtest), each `grafana_rule_group` as the
  Grafana alerting provisioning file it stands for. A backtest keeps only
  those rule groups of it: the recipe cuts the rest out before anything
  leaves the job, and serve's backtest port keeps nothing else, as a
  plan's JSON holds every resource's values, sensitive ones too.
- `lapilli demo --serve` holds three teams and a proposal waiting for
  approval, its backtest with it, to look at: made by the demo itself in
  its own ledger, which every request still only reads.
- docs/serve.md's Sizing: the ledger on disk and serve's memory, measured
  on SQLite and on Postgres by rules, firings a week (up to a million) and
  weeks kept, and how to measure them again (`LAPILLI_SIZING=1`). A
  Postgres ledger takes about three times SQLite's a firing.
- `--check-no-data=false` skips, of the steps after the replays, only the
  one that asks whether the rules that never fired read anything. The
  chart's CronJob takes `failUnreplayed`, `failNothingReplayed` and
  `checkNoData`.
- The chart runs the recipes for `lapilli serve`, each off until turned
  on: `serve.jobsAPI.copy` copies incidents from a tool (PagerDuty,
  incident.io, Jira and the rest of recipes/incidents) on a schedule, and
  `serve.changes.pullRequests` opens the pull requests for the changes
  people approved (recipes/changes). The recipes ship in the chart; the
  jobs take an image of yours with sh, curl and jq (and git), each
  Secret's one key they read, and `/tmp` held to `tmpSizeLimit`.
- `--match-strict` (verdict, serve, the chart's `matchStrict`): a firing
  that lacks a `--match` label the incident has doesn't catch it. Without
  it, as before, such a catch counts, and is now said: `(no service)` in
  the incidents table, a line naming the rules, `lacked` in `--json`, and
  the same mark in serve's pages.
- serve badges a rule whose judged firings caught nothing `Caught
  nothing`, not OK, and says the noisy bar beside a rule's badge; the
  chart takes `noisyFirings` and `noisyPrecision`.
- `lapilli demo --write DIR` writes the sample's rules and incidents, and
  a README with the command to run them against your own Prometheus.
- `.github/workflows/sizing.yml`, run by hand, measures the ledger and
  serve's memory on SQLite and Postgres (docs/serve.md, Sizing).
- docs/design/rendered-changes.md proposes writing an approved change to
  a rule rendered from a chart into the values file it comes from.
- Documentation: MAINTAINERS.md's table gives each maintainer's GitHub ID
  and organisation; the README and the FAQ say how the replay is checked
  against real servers and which part is the core and which is
  `lapilli serve`; the FAQ compares Lapilli with promtool, pint, vmalert's
  replay and Grafana's backtesting; the ROADMAP sorts its areas into the
  two.
- `test/e2e/kubernetes-rules.sh`, a CI job and weekly in `upstream.yml`:
  the replay checked against what a real Prometheus fired, on a real rule
  set. kube-prometheus-stack's default alert rules, as shipped, run on a
  kind cluster with failures made on purpose; `--check-replay
  --loaded-rules` must find every firing alike, among at least five alerts
  that fired, and the run's page in GitHub Actions says how many.
- **Classic conditions over Loki are replayed**: a Grafana rule whose
  classic condition reads a Loki query is replayed, each reducer worked out
  from the points of the range query Grafana reads, as Grafana's classic
  conditions do it (NaN, null, no data, every reducer, evaluator and
  operator), held to the Prometheus path by a test and checked against a
  real Grafana and Loki in `test/e2e/loki.sh`.
- **A Grafana rule on Prometheus with a Loki query beside its condition is
  replayed whole**: the Loki query is asked too, through Grafana's data
  source proxy, so its no data is seen; serve's backtest port takes such a
  rule on the Loki data sources the rules in effect read. A Loki query there
  that Loki refuses makes the rule replayed in part, not no data.
- **Loki's rules are changed as every other rule is**: a proposal for a rule
  of a Loki ruler's rule files (`--loki-rules`) is applied by the git path,
  a pull request that edits the one rule and keeps every other line,
  backtested in LogQL; a new rule can be added to a group, or a new group,
  of a Loki ruler's namespace `--changes-in` names as `loki:NAMESPACE`,
  through its API, or of a Loki rule file by the git path, held to its
  expectations (`loki` in MCP's `places` and `propose`). A Loki rule file
  isn't a place with `--loki-loaded-rules`, as Loki then runs what its API
  holds; a `keep_firing_for` isn't added to a Loki rule file, which Loki
  drops.
- A group read from a rule file is placed where the history's rules API (or
  a Loki ruler's) runs the group of its name, at the interval the API says
  where the file gives none, in `lapilli verdict` and `lapilli serve`.
  Prometheus's hash is checked against the groups' own last evaluations
  (Prometheus 3's and 2's layouts, paths of any length, taken only when
  three groups that ran agree and none disagrees); a name listed twice or
  not at all, or a hash that disagrees, is replayed on the steps as before,
  and the run says why. Against a VictoriaMetrics whose rules API is
  vmalert's, a group is placed where vmalert runs it (on whole intervals,
  or its `eval_offset`), so the same file's firings there are a few seconds
  from those against Prometheus, as vmalert's own are. verdict asks the
  rules API once a run, and says so on stderr when the history has none.
- Grafana's rules are checked against the external Alertmanager Grafana
  sends to, read from its alerting API (`admin_config`, `alertmanagers`), in
  `lapilli verdict --check-content`, serve and `lapilli preflight`
  (`grafana.alertmanagers`): with `--alertmanager-config`, its routes and
  templates, as a ruler's rules are. When Grafana sends to more than one,
  the data sources are named, as Lapilli takes the one given for all; a
  Grafana that answers otherwise is checked as before and says so.
- `lapilli preflight --queries` asks each rule's expression once, as an
  instant query at now, where it is replayed (the history, the clouds'
  PromQL, `--loki`, Grafana's Loki data sources, a Loki query beside a
  condition through Grafana's proxy), within the replay's limits
  (`--max-rate`, `--max-samples`, `--parallel`), and names each rule whose
  query fails and why (parse error, unknown function, too many samples,
  timeout): items `queries.history`, `queries.loki`, `queries.grafana`. Off
  by default; writes nothing.
- `--record-by-week` (chart `serve.recordByWeek`) keeps serve judging a
  firing by its week's record, as before 0.72. It judges no firing again:
  the stones don't depend on the record.
- `test/e2e/measure.sh` and the `measure` workflow, run by hand, take the
  replay's measurements kept as test data at any version of Prometheus,
  Grafana and Loki into `internal/replay/testdata`, one server of each
  running every recording's rules; `record` against servers kept up asks
  them again in minutes. Each recording's period is in its `period.json`,
  and the real-server tests replay every recording directory present. The
  recordings are taken again at the pinned versions, with the rules of the
  shapes above among them, and kept at the latest too (Prometheus 3.16.0,
  Grafana 13.3.2 and Loki 3.7.10, recorded by the workflow on a runner where
  no server missed an evaluation). `record` says how many evaluations each server
  missed in the periods, as a host too busy to run them on time makes them
  miss, and a recording whose test then fails says to record again on a
  host less busy: what a server fired after a missed evaluation is unlike
  any replay.
- `lapilli_incidents` and `lapilli_impact_windows` in serve's `/metrics`
  take a fourth state, `unreplayed` (below); the other three keep their
  meaning.
- The docs in Japanese, at lapilli.dev/ja/docs, from `docs/ja/`: the docs'
  home, Getting started, the glossary and the FAQ, the pages in Korean; a
  page not translated is listed with its Japanese title and opens in
  English, and the Japanese front page links to them. The Korean Getting
  started, glossary and FAQ are of the English as it is now; they had said
  the English had changed since.

### Changed

- **A headline number's change reads one way** ([docs/design/ui.md](docs/design/ui.md)). The
  overview's tiles said their change on the week before as "-3 pt", "+12%" (a share of the
  week before's firings), "+2" and "-4m"; each now says which way it went and by how much,
  in the number's own unit, with the week before's number: "Down 3 pt from 15% the week
  before", "Up 12 from 98 the week before", "Down 4m from 8m the week before" (Korean
  "전주 15%에서 3%p 감소"). The firings' change is the difference of the counts, no longer a
  share. Outcomes' then and now, a rule's panel and a proposal's backtest table say theirs
  the same way ("Down 24", "no change", "Up 2, worse"); better and worse keep their words
  and colours. The time to catch is shown without a plus ("4m").
- **Who is signed in, and Sign out, are in a menu at the top right, and the languages are one
  picker beside it.** They were in the footer, at the foot of every page, and the languages
  a list at the sidebar's foot that couldn't grow. The picker's first choice, "Browser's
  language", forgets the one chosen (`/lang?to=auto` deletes the `lang` cookie), so the page
  follows the browser's again. The footer keeps the version and the time zone.

- **Names made one thing, before 1.0** ([docs/upgrading.md](docs/upgrading.md#renamed-names)).
  Each flag and chart value renamed is taken under its old name until the next major
  release: stderr says `--old is --new now` for a flag, and the chart's NOTES says so for a
  value.
  - Serve's incoming notifications are `--notifications-listen`, `--notifications-token-file`,
    `--notifications-grafana-secret-file` and `--notifications-sns` (were `--notify-listen`,
    `--notify-token-file`, `--notify-grafana-secret-file`, `--notify-sns`); the outgoing
    `--notify-webhook-file`, `--notify-webhook-secret-file` and `--notify-slack` keep theirs.
  - The jobs' port is `--jobs-listen` (was `--incidents-listen`), the chart's `serve.jobsAPI.port`,
    `.from` and `.copy` (were `serve.incidentsAPI.*`); the incidents' own token stays
    `--incidents-token-file` and `serve.incidentsAPI.tokenSecret`.
  - `--changes-pause-file FILE` (was `--changes-paused`) takes a file's path, and refuses `true`,
    `false`, `1`, `0`, `on` and `off` under either name, which read as a switch and would have
    made a file of that name.
  - `--changes-recipe-token-file` (was `--changes-token-file`), the chart's
    `serve.changes.recipeTokenSecret` (was `serve.changes.tokenSecret`).
  - `--prometheus-token-file` (was `--bearer-token-file`), `--extras-timeout` (was `--extras`),
    `--fail-no-expectation` (was `--expect-required`), `--google-project` (was `--gcp-project`),
    `--redact-key-file` (was `--redact-key`), `--webhook-dest-secret-file` (was
    `--webhook-dest-secret`), `--oidc-hosted-domains` (was `--oidc-hosted-domain`).
  - The backtest action's inputs `gcp-project` and `expect-required` are `google-project` and
    `fail-no-expectation` (the old ones carry a `deprecationMessage`; given with the new name, an
    error), and the recipe refuses and hides the old flag names as it does the new.
  - The chart's `gcpProject` is `prometheus.googleProject`; `serve.checkContent.alertmanager` is
    the top-level `alertmanager`, `serve.checkContent.scrapeInterval` the top-level
    `scrapeInterval`; `serve.roles.groupsHeader` is `serve.groupsHeader`. An old key at its 0.71.0 default (as
    `helm show values` and `--reuse-values` keep it) is ignored; one set otherwise moves to the
    new key when that is at its default, and a block set under its old key is laid over the new
    key's default block, key by key (a key it leaves out keeps the new default, an empty value it
    holds is kept as given). Both set and differing (after the old block is laid over the new
    key's default) is an error naming both. The two keys 0.71.0 read in serve mode alone are
    moved in serve mode alone: in cronjob mode they stay ignored, as they were, and NOTES.txt says
    so. The chart fills each key missing from the values from its own `values.yaml` only on a
    `helm upgrade --reuse-values` from an older release, whose marker `chartValues` is not the
    chart's own (a release writes its version there, git has `dev`; older releases hold an older
    marker or none; not a value to set, and never `--set chartValues=null`). With its own marker,
    nothing is filled and `--set key=null` removes a default as before.
  - `helm upgrade --reuse-values` from 0.71.0 keeps its `image.digest`, which this chart's
    image isn't: a released chart with `image.tag` empty (or equal to its own version) and an
    `image.digest` that is another release's now fails, where it ran 0.71.0's image under this
    version's name. The message offers the chart's own digest (`image.digest=<it>`), or
    `image.digest=""`, or another `image.tag` too; a tag equal to the chart's version is refused
    with another release's digest, and another tag with the chart's own digest is refused. The
    guards apply only to the chart's own `image.repository`: a mirror with its own digest and tag
    renders.
  - When the chart fills missing keys (the values' marker differs from its own), `NOTES.txt` says
    so and that a `null` removes no default in that release: drop `chartValues` from copied
    values files, upgrade with `--reset-then-reuse-values`. `--reuse-values` between two git
    checkouts (both `dev`) is not covered.
  - Renamed before release, so with no old name: `--notifications` is
    `--notifications-worked-out` (`=false` turns the notifications worked out off), and the chart's
    `notifications.enabled` is `notifications.workedOut`; `--depends-count-missed` is
    `--depends-count-catches`.
- **The Alertmanager's configuration is read with the content check off, for who a firing
  reached.** `--alertmanager-config` with `--check-content=false` is no longer refused, and the
  chart's `alertmanager` is passed and mounted whether or not `serve.checkContent.enabled` is on,
  and in cronjob mode too: with the content check off it is read for the notifications worked out
  alone. It is refused when both are off (`--notifications-worked-out=false`,
  `notifications.workedOut: false`), as it would be read for nothing. `lapilli preflight` allows
  the configuration's URL for the notifications as for the content check, reads the configuration
  (`content.alertmanager`) and counts the hourly `alertmanager_config` requests whenever either
  reads it. A configuration `lapilli verdict` can't read is an error, exit 1, with the content check
  on or off, as before (so a run never silently reports no pages). `lapilli preflight` reports a
  routing that can't be read (a route to an undefined receiver, a matcher that doesn't parse) with
  its reason, and with the content check off doesn't report missing `templates:` globs. The hourly request estimate counts the Alertmanager API twice an hour (48 a day) with both the content check and the notifications worked out on, and Grafana's routing reads (four an hour, `grafana_routing`) for the notifications worked out.
  `scrapeInterval` is passed whether or not the content check is on.
- **`serve.rulesHistory.every` takes a duration** such as `5m` or `1h30m`, as well as an integer of
  seconds (30 seconds at least, as before). The comments of `report.timeZone` and `timeZone` in
  `values.yaml` say which one wins in each mode, and those of `serve.trackers.tokenSecret`,
  `serve.firings.quietAfter` and `serve.notify.lang` name the flags they pass.
- **Names renamed before 1.0, the old ones still taken** ([docs/upgrading.md](docs/upgrading.md#renamed-names),
  [docs/numbers.md](docs/numbers.md#what-stays-the-same)). What to do: take the new name when
  you next touch the job or the dashboard.
  - `GET /api/v1/activity` takes `from` and `to`, as every other period on the API does (`to`
    takes a day whole, as `until` did); `since` and `until` are still taken as their old names,
    and a name given with its old one is a 400 naming both. The activity page's form and its
    export links use `from` and `to`.
  - `GET /api/v1/expectation-drafts` and `/api/v1/expectation-drafts/{id}` are
    `/api/v1/expectations/drafts` and `/api/v1/expectations/drafts/{id}`. The old paths answer as
    the new ones do, with `Deprecation: @1795651200` (2026-11-26) and a `Link` to the new path.
  - A window's repeat day is `repeat_on` in the bodies of `POST` and `PUT /api/v1/windows` and
    `/api/v1/marked` and in the `--marked` file, as `--config` has it; `on` is taken as its old
    name, and both is an error naming both. What the API gives back has `repeat_on`, with `on`
    beside it, and so has the row in a `window.kept` or `marked.kept` notice.
  - `not_judged` (a rule's and a firing's in the report, the summary's rows and the goals'
    `people`, MCP's `rules`) is written beside `judged_by_nothing`, the same value: it counts what
    a rule entry judges by nothing, not what the pages call "Not judged". The old one goes in the
    last 0.x minor release. The summary's `schema` stays 1.
  - preflight's `asked.by` has `on_call`, as `estimates.beside.by` has it, with `on-call` beside
    it for the same requests, until the last 0.x minor release.
  - `lapilli_rule_pages{basis="worked out"}` and the summary's, MCP's `paged.basis` values
    `worked out` and `recorded and worked out` are `worked_out` and `recorded_and_worked_out`
    (new in this release, so no old value is kept). The pages say it as before.
  - The report's durations stay in nanoseconds (`_ns`), the one JSON surface that is; the
    others give seconds. [docs/numbers.md](docs/numbers.md) says so.

- **The real-server end-to-end scripts say when a server missed evaluations.**
  `test/e2e/loki.sh`, `compat.sh` and `kubernetes-rules.sh` read each server's
  missed-evaluation counter (Prometheus's, Loki's, Grafana's and vmalert's) when the
  checked period starts and when it ends, and say `evaluations missed in the period`;
  a failed comparison with some missed says the servers missed them, as no replay does,
  and for Grafana prints its "Tick dropped" log lines for the period. It still fails:
  a late evaluation explains a difference and doesn't excuse it. `measure.sh` counts
  with the same `test/e2e/missed.sh`.
- **lapilli serve reads a merge group's pull request number and branch
  from its ID token's `ref`**, the queue's, and only from there: a body
  that names another number is refused, and a `merge_group` token whose
  `ref` isn't a queue's is refused (401). A group the queue built again
  takes the place of the one waiting for the same repository, branch and
  pull request, never a pull request's own backtest's place, nor a pull
  request's newer commit a group's. A backtest takes the place only of one
  of another commit and, of a GitHub workflow, of an earlier run of it (by
  the token's `run_number`), so that two jobs of one run, a run that asks
  late, or an earlier run run again take no newer one's place. **Ledger
  schema 033** (`backtests.queue_branch`, `workflow`, `run_number`), one
  start, no backfill: keep a copy of the ledger first.
- **The command's tests in a third of the time**: `cmd/lapilli`'s tests
  that keep to their own files and stand-ins run in parallel, as serve's
  have since 0.71.0, and a test that sets one of the command's hooks sets
  it back as it ends (`cmd/lapilli` 316 s → 87 s here, run back to back).
- The end-to-end tests on kind of the chart's CronJob and of the whole
  service (`test/e2e/run.sh`, `service.sh`) take `PROMETHEUS_IMAGE` and
  `GRAFANA_IMAGE`, as the scripts in Docker have since 0.71.0, and
  `upstream.yml` runs them weekly against the latest release of each
  with the rest.
- `lapilli verdict` exits 1 when no rule was replayed, every replay
  having failed (the history unread, or too many labels on a rule's
  series): the report judges nothing. `--fail-nothing-replayed=false`
  exits 0 as before; a run that replayed some rules, and `--compare`, are
  as they were.
- Schema 032: a catch keeps the labels its firings lacked. serve's first
  start after the upgrade judges every firing again once, as a
  `--linked-only` change does. A release before it can't open the ledger
  after: keep a copy first (docs/upgrading.md).
- **lapilli serve judges a firing by its day's record, not its week's.** A
  day's firings are judged when an incident was on in the day or in the two
  weeks before it, or `--impact` was read over all of it, in `--tz`, as
  `lapilli verdict` judges by the firing's own time; every page, report,
  digest, backtest through serve, MCP tool and API number follows.
  Upgrading, a firing on a day before an incident started in its week is no
  longer judged, and one after an incident's two weeks ended in its week is
  judged no more; nothing is judged again, as the stones don't depend on the
  record. `record_by` (`"day"` or `"week"`) says which in `GET
  /api/v1/settings` and a report's `settings`; `record_by_week` is true only
  by week. `--record-by-week` keeps the old (docs/upgrading.md).
- `--linked-only` is a setting an admin changes on the Settings page and
  through `POST /settings`, the flag defining it until then; a change judges
  every firing again once, across replicas, and the activity log says who.
- `POST /settings` leaves a setting alone when its key isn't in the form,
  where it went back to its flag; an empty value or `reset-KEY` still gives
  it back.
- "Woken for nothing" is counted by the firing's start day, the days not
  judged left out, whether or not notifications are kept.
- `lapilli demo --serve` replays the week before too, and judges by the day
  as serve does: its incidents are the sample's, the week before's with
  them.
- **lapilli.dev's text at 16px**, as most docs sites have it, from 17px,
  with its headings a step further apart. A docs page's sections sit right
  after its text, with no empty band between, and the pages' list lines up
  with the header's mark. The front page is 72rem wide, its screenshots,
  cards and commands across it, its text still at a reading width.

### Fixed

- **A firing of Grafana's NoData state was counted alike with the alert's while alerting**
  that started near it, as their labels agree on what both have, in `lapilli verdict
  --check-replay` and in serve's check of each day: one is alike only with another of its
  state now, so a replay that has an alert alerting where Grafana said NoData differs.

- **`lapilli verdict` with the content check off** read the Alertmanager's templates and said
  the template files it passed over on stderr, unlike serve and preflight, and labelled the
  `--alertmanager-config-before` file's lines `--alertmanager-config`: the configuration is read
  for its routing alone now. Preflight says `notifications.routing` is lost when the content
  check's own read of the configuration fails too.

- **A Grafana rule whose math expression Grafana's lexer refuses** (`$A>-1`,
  `$A--$B`, `^`, an operator its lexer reads as one run of characters) was
  replayed as though Grafana read it; Grafana errors on it at every
  evaluation, so it is listed as not replayed, as a rule Grafana can't
  evaluate is. Grafana's power is `**`, which isn't read either, and neither is a function Grafana doesn't have (`exp()`).
- **Two backtest jobs of one pull request's commit took each other's
  place** through serve (since 0.61.0), with two `KEY`s in one workflow:
  the first failed under its gate while serve was busy. A backtest takes
  the place only of one of another commit now.
- `RENDER` and `RENDER_PREPARE` run without GitHub Actions' step files
  (`GITHUB_STEP_SUMMARY`, `GITHUB_OUTPUT`, `GITHUB_ENV`, `GITHUB_PATH`,
  `GITHUB_STATE`); a merge group's branch is 255 characters at most in
  `github.sh`, as in serve.
- `lapilli serve` restarted on a ledger it had run panicked on its
  overview's first view, before anything was written again.
- One rule left out of a report is said to be ("is left out"), in the
  text and in a pull request's comment.
- `lapilli preflight` counted none of the Loki requests of a Grafana rule
  that reduces a LogQL query (its estimate was 0 where the replay asked
  about 2,000 over 28 days); it counts them as the replay asks them.
- **A firing on when the period starts**, that ends before its placed
  group's first evaluation in the period, is no longer left out: a placed
  group (Prometheus's with `--loaded-rules` or placed from the rules API,
  Loki's) is replayed from its last evaluation before the period, in a
  backtest, a longer or shorter `for` tried and near misses alike. A rule
  new after that evaluation has no firing from it.
- **A Grafana rule whose condition comes to a time series** (a threshold or
  math straight on a range query, Prometheus's or Loki's) is listed as not
  replayed, with why: Grafana errors at its every evaluation ("only reduced
  data can be alerted on"), and the replay had it firing.
- **An incident that started before what serve replayed** isn't counted
  missed: a fresh serve given last month's incidents said every one was. It
  is shown apart ("before what was replayed") on the overview, the incidents
  page and chart, the summary API (`unreplayed`), goals, the digest,
  reports, MCP and `/metrics`.
- The heatmap's long rule names are whole for the keyboard, the pointer and
  a screen reader, not only in a title.
- A firing that ended where a period starts was counted in it: a report
  judged it, and `--check-replay` counted a replayed one Prometheus's or
  Grafana's history can't have (a rule with `for` or `keep_firing_for`
  replayed from before the period). A firing's end is the first time it
  was off, as `--check-replay`'s count of errors already had it.
- **The test of replicas' tables changed outside the writers' turn** failed on
  a busy runner: it bounded how long a write waited (250ms) while a start
  waited on a dump. It, the test of a migration waiting on a dump and the
  tests of a start, a take, a write and a refresh during a dump or a held
  turn now decide by what completes while the dump's lock is still held (a
  write that waited would fail at its deadline), not by elapsed time.
- **The replicas end to end (`e2e-replicas`) is a required check.**
  Its downloads (Go's modules, the image's base, kind, kind's node image) are
  tried up to three times, a cluster that failed to come up is deleted and
  made again, and Go's modules and build stay cached by `setup-go`. It still
  runs only on a pull request that reaches it (`test/e2e/reached.sh`).

## [0.71.0] - 2026-11-24

Backtests in a merge queue: GitHub's `merge_group`, and GitLab's merge
trains judged against the commit they are a merge of. `FAIL_MISSED`
through serve compares a rule the pull request leaves alone as parsed
PromQL, so a serve reading its rules from Prometheus's API keeps its
catches. The tests take less than half the time, and none is bounded by
how long something took. `--alertmanager-config` in the chart and in
`lapilli preflight`, `RDATE` in on-call feeds, `--prometheus-tenant`, and
Grafana IRM's incidents read by `QueryIncidentPreviews`. And what merged
since 0.70.1: the Korean docs, `docs/data.md`, incidents not known apart
from those missed, staticcheck in CI, sign-in taking a compact token
alone, and the real-server tests run weekly against each server's latest
release. No schema change: the ledger stays at 031.

Before upgrading:
- **A GitHub workflow that adds `merge_group`**: the fork-skipping `if:`
  (`github.event.pull_request.head.repo.full_name == github.repository`)
  skips a group's run too; write `github.event_name == 'merge_group' ||
  ...`. A group behind another needs `fetch-depth: 0`.
- **A required backtest in GitHub's merge queue** needs the queue's "Only
  merge non-failing pull requests" on, GitHub's default: GitHub doesn't
  say whether a group's base behind another group is the branch's commit
  or the head of the group ahead, and with it off, a pull request whose
  group failed merges once a group behind it passes.
- **A GitLab job in merge train pipelines** whose checkout isn't the
  pipeline's commit, or on a train whose commit isn't a merge of the
  source branch's head (a squash), now fails, gates or not: check out the
  pipeline's commit, or leave the job out of `merge_train` pipelines.
- **A trust entry for a merge queue's ID token** names `event_name:
  merge_group` in an entry of its own; one naming `pull_request` refuses
  it.

### Added

- **Backtests in GitHub's merge queue** (`merge_group`): the merge
  group's head (`merge_group.head_sha`) is backtested against the base the
  event names (`merge_group.base_sha`), `INCIDENTS` and the expectations in
  place read from the base; no comment, the report going to the job's log;
  with `STATUS`, `lapilli/backtest` (`(into BRANCH)` for another branch)
  on the head, pending then success or failure. `FAIL_MISSED` and
  expectations hold it as they hold a pull request. An event with no
  `merge_group`, a `GITHUB_REF` that isn't the queue's ref for the branch,
  or a `GITHUB_SHA` that isn't the head is refused with no status; a
  checkout that isn't the head, a base that isn't its ancestor, or a
  `TARGET`, `TARGET_BRANCH`, `STATUS_SHA` or `PR` naming another fails the
  job, its status a failure on the head. A field of the event is read
  whole or refused.
- **lapilli serve takes a merge queue's ID token** only of a GitHub entry
  that pins `event_name: merge_group`; one pinning `pull_request` refuses
  it.
- **`serve.checkContent.alertmanager` in the chart**
  (`--alertmanager-config`): Alertmanager's configuration from a Secret or
  a ConfigMap, mounted whole at `/etc/lapilli/alertmanager/` with
  `templateConfigMaps` beside it, or a Mimir's or a Cortex's `url`, read
  with `rules.ruler.tenant` and `rules.ruler.tokenSecret`.
- **`lapilli preflight`'s `content.alertmanager`**: `--alertmanager-config`
  read once as serve reads it, with how many receivers, template files and
  templates it found; a problem when it can't be read, or when serve would
  pass something over.
- **`RDATE` in on-call feeds' events**: dates, days and periods added to
  an event, with an `RRULE` or without one, each once, `EXDATE` applied,
  `TZID` as `DTSTART`'s; bounded as a repeat is.
- **`--prometheus-tenant ID`**, the history's own tenant, sent to
  `--prometheus` alone in place of `--ruler-tenant`, in verdict, serve,
  preflight and `lapilli impact suggest`, taken out of a backtest's
  comment as the other tenants are; the chart's `prometheus.tenant`.
- lapilli.dev's docs pages list their own sections beside them, with a
  search over every page's title, description and sections, the pages
  before and after at the end, and a button for the dark theme. Nine long
  pages have the headings to go with it.
- The docs in Korean, at lapilli.dev/ko/docs: Getting started, Words and
  Questions so far, from `docs/ko/`; a page not yet translated is listed
  with its Korean title and opens in English, and a translation older
  than its English page says so.
- `docs/data.md`: what each command reads, what `lapilli serve` keeps and
  for how long, who can reach it, what it sends where, and what it can
  change, on one page.
- `lapilli verdict` says an incident is not known, rather than missed,
  when no rule replayed caught it while a rule couldn't be replayed, which
  may have: `? not known` in the table, `? N of M incidents can't be
  judged: no rule was replayed.` (or `had no alert from the K rules
  replayed`) under it, `"not_known": true` in `--json`, and
  `lapilli_incidents_not_known` in `--metrics`. `missed`, its metric and
  the exit status are as they were; `--fail-unreplayed` still fails such a
  run.
- The rules that never fired are named, ten at most, and a step after
  the replays (skipped with `--extras 0`) says which of them compare a
  value that had no series at all in the period, so couldn't have fired:
  a metric's name mistyped, a recording rule the history doesn't keep
  (`no_data` in `--json`).
- `lapilli verdict` warns, and `lapilli preflight` says `rules.sources`
  (unknown), when Grafana's rules query more than one PromQL data source:
  every one is replayed against `--prometheus`, read from files or with
  `--grafana`.
- docs/getting-started.md says what each column of the report says.

### Changed

- **The tests take less than half the time.** serve's tests that keep to
  their own ledger and server run in parallel, each ledger a copy of one
  migrated once, and CI runs them eight at once. No test is bounded by how
  long something took: the budgets count statements, rows, allocations,
  waits begun and the work a reader does, so a slow runner fails none. A
  token's writes are let through by the server's wall clock, which tests
  can stop.
- **Grafana IRM's recipe reads `IncidentsService.QueryIncidentPreviews`**,
  as its reference deprecates `QueryIncidents`: newest first, stopping at
  `SINCE`; a page out of order with `SINCE`, an answer with no list of
  incidents, or one that says there is more but not where, writes nothing;
  a `SINCE` that isn't a date is refused.
- With `FAIL_MISSED` through serve, a group with no `interval` is still
  another rule where serve can't read Prometheus's configuration, the
  configuration doesn't state `evaluation_interval`, serve's latest rule
  runs at another interval, or the rules come from vmalert. A known limit:
  Prometheus's rules API doesn't report a group's `query_offset`, so a file
  group without one is the same rule as serve's, whatever offset the
  running group had; a file group with one is another, its catches not
  known. Grafana rules, Loki ruler rules and cloud alarms are still
  compared by their text.
- The real-server end-to-end scripts take `PROMETHEUS_IMAGE`,
  `GRAFANA_IMAGE`, `LOKI_IMAGE`, `THANOS_IMAGE`, `VICTORIAMETRICS_IMAGE`
  and `VMALERT_IMAGE`, and `upstream.yml` runs them weekly against each
  one's latest release; the pinned images are still what a pull request
  runs.
- serve's sign-in takes a token in the compact serialization only (what
  every identity provider issues); a JWS in the JSON serialization, or
  with spaces inside it, which go-jose's old parser let through to the
  signature check, is refused before it.
- Releases are gathered: at most two minor releases a week, each with
  what merged since the last; a fix for a vulnerability or for what a
  release broke is still a patch as soon as it is ready (GOVERNANCE.md).
  The README says where to read what stays the same across releases
  (docs/numbers.md, docs/upgrading.md), ROADMAP.md what comes first, and
  docs/design/README.md lists the design notes by area.
- CI runs staticcheck (`staticcheck.conf`), and what it found is gone:
  values assigned and never read, code nothing called, loops an `append`
  does, and go-jose's deprecated `ParseSigned`. Nothing a user sees
  changes.
- The rules whose replay failed are listed once, at the top, those that
  failed for the same reason on one line; counts of one say "1 rule"; the
  hours a report counts out of hours by name the zone as its first line
  does, not `Local`.
- `lapilli preflight`'s memory, with no firing read, says the caps' bound
  is the most a run can take, and no longer gives it as a request and a
  limit.
- Plainer errors: a file of incidents that is a list, a time that isn't
  RFC 3339, and YAML's own errors on one line; `--rule` given a path
  points to `--rules`; two sources given are named; where the history is
  wanted, a managed Prometheus's URL with `--auth` is said beside
  `--gcp-project`.

### Fixed

- **`FAIL_MISSED` through serve compared a rule the pull request leaves
  alone by its text** (0.70.0). A rule serve reads from Prometheus's rules
  API (`--loaded-rules`), Mimir's or Thanos', which print the expression
  again (`sum(x) by (a)` as `sum by (a) (x)`, `300s` as `5m`, `for` in
  seconds), had every incident that hung on it "not known", so the job
  failed whatever the change. It is compared now as parsed PromQL, equal
  only where Prometheus and MetricsQL both read the two alike; and a rule
  file's group with no `interval` is the group Prometheus's rules API
  reports, where serve read its `evaluation_interval` from Prometheus's
  `/api/v1/status/config` (once a day) and serve's latest rules of that
  name run at it. A `\r` in a comment, a zero offset where Prometheus takes
  no offset, a second `offset` or `@`, and what the two dialects read
  apart, are compared by their text.
- **A GitLab merge train's pipeline was compared with
  `CI_MERGE_REQUEST_TARGET_BRANCH_SHA`**, which GitLab documents for merged
  results pipelines alone: what the merge requests ahead in the train
  changed was judged as this one's, and `INCIDENTS` read from the target's
  head. It is compared now with the first parent of the train's merge
  (`CI_COMMIT_SHA`), `INCIDENTS` that commit's.
- docs/serve.md's sidebar lacked Marked windows and Goals; the chart's
  values said serve takes one rules ConfigMap, where it mounts them all as
  one directory; docs/verdict.md says which cloud paths were run against
  real accounts.

## [0.70.1] - 2026-11-24

Eight modules a patch or a minor release on, `go-jose` among them, which
checks the tokens `--proxy-jwt` and `--oidc-issuer` take; and two places
where the docs said less than the code does. No schema change: the ledger
stays at 031.

### Changed

- `github.com/go-jose/go-jose/v4` 4.1.7 → 4.1.9,
  `github.com/Azure/azure-sdk-for-go/sdk/azcore` 1.23.3 → 1.24.0,
  `github.com/aws/aws-sdk-go-v2` 1.47.5 → 1.47.8 (its `config`,
  `credentials` and `service/sts` with it),
  `github.com/oracle/oci-go-sdk/v65` 65.128.0 → 65.129.0 and
  `golang.org/x/oauth2` 0.37.0 → 0.38.0. govulncheck finds nothing the
  code calls, before or after.

### Fixed

- `lapilli serve -h` said `--changes` needs `--user-header` and
  `--admins`; it needs a sign-in (`--user-header`, `--oidc-issuer` or
  `--proxy-jwt`), roles (`--editors` or `--admins`) and who approves
  (`--approvers` or `--admins`), as serve has refused it without them
  since 0.30.0. And docs/limits.md said a backtest can't stop a merge,
  where a job's failure and its commit status (`STATUS`) do.

## [0.70.0] - 2026-11-23

`lapilli demo --serve`, the sample in serve's own pages. `FAIL_MISSED`
through serve. A Loki ruler's rules changed through its API, as a
Mimir's are, and Loki in the chart. Alertmanager's templates checked
beside each rule. YouTrack and Jira Data Center among the trackers, and
comments' edits followed both ways. Monthly and yearly repeats, and a
feed's own `VTIMEZONE`, in on-call feeds. serve saying a firing came
after its incident ended, as verdict does. And what made two end-to-end
tests fail now and then, both in serve itself: a version of the rules
added while serve read the history, and a replica that closed its ports
the moment it was told to stop. No schema change: the ledger stays at
031.

Before upgrading:
- **`--changes-in loki:NAMESPACE` beside `--loki-loaded-rules`** names a
  namespace of Loki's ruler now, no longer a `--ruler` namespace of that
  name. Without `--loki-loaded-rules` it is `--ruler`'s, as before; with
  no `--ruler` either, it is refused. An `--alarms` source named `loki`
  is refused beside `--loki-loaded-rules` with changes on (Changed).
- **With `serve.replicas` above 1**, each pod answers for five seconds
  after it is told to stop (`serve.shutdownDelaySeconds`), not ready
  meanwhile, and its `terminationGracePeriodSeconds` is 35. A request
  under way when serve stops now finishes, ten seconds at most, where it
  was ended at once (Fixed).
- **`FAIL_MISSED` with `LAPILLI_SERVE`** needs serve 0.70.0: an older one
  refuses it, and the job says so. With replicas of both, a job asks
  again once every one is. A rule serve reads from an API that writes
  its expression otherwise than the repository's file does
  (Prometheus's rules API, `--loaded-rules`) leaves the incidents that
  hang on it not known, failing the job: compared by text, the two
  differ.
- **Where `--ruler-tenant` or `--loki-tenant` is set**, a ruler proposal
  made before 0.70.0 kept no tenant and is refused once approved:
  propose it again. One left applying across the upgrade stays applying
  until an admin closes it: let changes settle before upgrading (Fixed
  (security)).
- **An on-call feed's turn at a time the clocks skip or show twice** is
  read as RFC 5545 reads it: 02:30 in the gap is 03:30, where 0.69 made
  it 01:30; of two, the first. A repeat is worked out on the clock it is
  written on, so a weekly 09:00 turn stays at 09:00 when the clocks
  change (Fixed, Changed).
- **The range check asks Prometheus once more**: for a selector that
  names no job, `count by (job)` of its metric, at most 200 a pass, kept
  a day in serve (Changed).

### Added

- **`lapilli demo --serve`: the sample in `lapilli serve`'s pages.** It
  runs serve's own code over the same sample as `lapilli demo`: its
  rules and the incidents of its week in a temporary directory, its
  history answered in the process, a ledger kept beside them. serve's
  clock is pinned to the end of the sample's week (Monday 16 November
  2026, 00:15), so its first run replays the week, its digest works it
  out, and every page and `/api/v1/report` shows it as the last whole
  week, with the numbers `lapilli demo` prints. Read-only, every write
  refused; on a loopback address alone (`--listen`, `127.0.0.1:0`, a
  free port, by default; a name or any other address refused); reading
  no Prometheus and sending nothing. It says the URL to open; Ctrl-C or
  SIGTERM ends it and removes the directory (docs/design/demo.md,
  "`--serve`").
- **`FAIL_MISSED` through `lapilli serve`** (`LAPILLI_SERVE`, the
  action's `fail-missed` with `serve-url`): the job asks serve to hold
  the change to its incidents (`fail_missed`), and fails, once the
  comment says which, when the change leaves an incident a rule caught
  before it caught by no rule after it, as `--fail-missed` does. The
  rules that differ are judged by the backtest; the rules left alone by
  serve's record of what caught each incident, not replayed, a catch
  counting only where the rule was written then as it is after the
  change: a rule the pull request holds alike, or one serve runs now
  that it doesn't hold, keeps an incident caught only so. One that a
  rule left alone may have caught unrecorded (serve doesn't run it as
  the pull request has it, its runs don't cover the incident's time, it
  couldn't be replayed then), that a rule written otherwise then caught,
  or that a rule after the change that couldn't be replayed may catch,
  is not known (`missed_not_known`), and fails the job held to it.
  Nothing more is asked of the history (docs/design/backtests.md, step
  4).
- **A Loki ruler's rules changed through its API** (`lapilli serve
  --changes`): a rule read with `--loki-loaded-rules` is written, once
  approved, through Loki's ruler's configuration API
  (`/loki/api/v1/rules`), as a Mimir's is. Its group is read again just
  before (the rule changed since is refused as stale; others changed
  only, it is made afresh), kept for undo, sent once with
  `--loki-tenant` as `X-Scope-OrgID`, and read back. It is done only in
  the namespaces `--changes-in` names as `loki:NAMESPACE` (or
  `loki:PREFIX*`), on a path of its own, `loki`, never sent to
  `--ruler`; `--changes-loki-token-file`, a token of its own, writes.
  `lapilli preflight` says what it may write is unknown
  (`changes.loki`). A Loki whose ruler keeps its rules in local files
  takes no writes. Run against a real Loki 3.7.9 in `test/e2e/loki.sh`
  (docs/design/rulers.md, "Loki's ruler").
- **The chart's Loki**: `loki.url`, `loki.tenant` and `loki.tokenSecret`
  (mounted as a file); `rules.loki.loaded`, or `rules.loki.configMaps`
  for its rule files; `serve.changes.lokiTokenSecret`. And
  `rules.grafana.history: false` for `--grafana-history=false`.
- **Alertmanager's notification templates checked beside each rule**
  (`--alertmanager-config FILE|URL`, for `lapilli verdict
  --check-content` and `lapilli serve`): the receivers a rule's alerts
  reach, by the route tree; a template that shows an annotation the rule
  lacks, names a label its alerts never had, or doesn't parse, which
  Alertmanager finds only when it sends (`notify_template_error`). Read
  from `alertmanager.yml` and its template globs, from a file in
  Mimir's, Cortex's or Amazon's form, or from Mimir's and Cortex's
  `/api/v1/alerts` with `--ruler-token-file` and `--ruler-tenant`: with
  `--ruler` and its token, on `--ruler`'s host alone. No field that holds
  a secret is parsed, and a templates glob that leaves the
  configuration's directory isn't looked through
  (docs/design/content.md).
- **The event recorder's file and Kafka outputs**, sent on to
  `/v1/alertmanager/events` by `recipes/events/alertmanager.sh`: from
  where it stopped, by an offset file or a Kafka consumer group (Kafka
  read with `kcat -J`), a file rotated (`dateext` too) or truncated read
  as it should be, in batches of 1,000 events and 1 MB at most, a batch
  answered 413 sent an event at a time. `MAX_READ` (16 MiB) bounds what a
  run reads, a line or a message longer passed over and said; one run at
  a time, by a lock beside the offset file; an offset that can't be
  written stops the run, exit 1 (docs/design/inputs.md).
- **YouTrack and Jira Data Center, among the trackers tasks are kept in
  step with** (`recipes/trackers/youtrack.sh`, `jira-dc.sh`; the
  chart's `serve.trackers.tracker: youtrack` or `jira-dc`). YouTrack's
  issues in one project, by its REST API and a permanent token; Jira
  Data Center's or Server's (8.14 and later) by REST API v2 and a
  personal access token, its text in wiki markup, the task's names and
  the comments posted from Lapilli in a `{noformat}` block, so that a
  PromQL selector's braces aren't a macro and `[~name]` mentions no
  one. Each does what the others do, comments both ways among it, tested
  against a stand-in for its documented API, not a real account.
- **Comments' edits followed, both ways** (`COMMENTS=true`, every
  tracker): a comment people wrote in Lapilli and changed after it was
  posted has the job's post changed, once; a comment from the tracker
  changed there after it was copied is taken again, as the tracker's,
  once. A post is compared as the tracker gives it back, so a save that
  changed nothing edits nothing; an edit the tracker refuses is said and
  passed over. A deletion isn't followed. The tasks token may change a comment
  copied in from the tracker, by a later edit alone (`PATCH
  /api/v1/tasks/{id}/comments/{comment}` with `text` and `edited`), and
  nothing more (docs/design/trackers.md, "Edits followed").
- **Monthly and yearly repeats in on-call feeds**, with `INTERVAL`,
  `COUNT`, `UNTIL`, `BYMONTHDAY` (`-1` the last day), `BYDAY` numbered
  in the month, or in the year without `BYMONTH` (`1MO`, `-1FR`),
  `BYMONTH` and `BYSETPOS`, as RFC 5545's table has them; a day a month
  doesn't have is skipped.
- **A feed's own `VTIMEZONE`**: a `TZID` that isn't an IANA name, such
  as Outlook's `Pacific Standard Time`, is read by the feed's
  `VTIMEZONE` of that name, wherever it is in the feed; with none, a
  Windows zone name is read by CLDR's table. An IANA name of a place
  (Europe/Berlin) still wins over the feed's `VTIMEZONE`; the feed's
  wins over IANA's names for a fixed offset (`EST`, `MST`,
  `Etc/GMT+5`). A `VTIMEZONE`'s rules are yearly, holding a day each
  year: a monthly or daily one, or one with a year of no day (29
  February), is refused, and the events in it are passed over and said.
- **`lapilli serve` says a firing came after its incident ended**, its
  condition having held in it, as `lapilli verdict` does: `late`,
  `fired_after`, `after` and `held_from` in `/api/v1/report`, the
  digest, a report's missed incidents, the Incidents page and an
  incident's own ("✗ nothing in time; after its end: RULE +1m"), a
  rule's page, and MCP, and `late_cut` where one read of them was cut
  short at its bounds.
- **With no record, how many of a rule's firings went well past its
  bounds is said of a range, and of comparisons joined by `and`, `or`
  and `unless`** (Grafana's `&&`, `||`, ranges and classic conditions
  too), where the bounds decide the condition; `bounds` says how many. A
  value beside them under `or`, or a comparison under `!` or inside
  arithmetic, is still left unsaid (docs/design/serve.md, "With no
  record: which conditions").
- **A `for` one log line can't outlast**, of LogQL's `count_over_time`,
  `rate`, `bytes_over_time` and `bytes_rate`, in a Loki rule and a
  Grafana rule's instant Loki query, a pipeline starting with a line
  filter, `|=` or `!=` alike, as of PromQL's counts of events
  (docs/design/loki.md).
- **`lapilli serve --shutdown-delay DUR`**, and the chart's
  `serve.shutdownDelaySeconds` (5, with more than one replica): how long
  a replica told to stop answers on, not ready, before it stops.
- **gitlab.sh tested in a merged results pipeline** (the temporary
  merge compared against the target branch's head, the status on the
  source branch's head) and in a plain merge request pipeline.

### Changed

- **The range check of a selector that names no job** counts with the
  longest interval of the jobs its series come from, asked once with
  `count by (job)`, and the finding names the job, where it counted with
  the global interval. When the query fails, as before.
- `--ruler-token-file` and `--ruler-tenant` go with
  `--alertmanager-config URL` too.
- **`/v1/alertmanager/events`: an event of more than 1,000 alerts is
  refused as 400**, as any event the path can't read, not 413, which a
  sender takes for a body too large and sends again. The webhook path,
  `/v1/alertmanager`, answers 413 as before.
- A rule from a Loki ruler's rule files says in its own words why it is
  for a person; a Loki rule from the API whose namespace isn't named says
  "its namespace in Loki's ruler isn't one --changes-in names, as
  loki:NAMESPACE".
- The chart refuses a Loki flag given in `extraArgs` beside `loki` or
  `rules.loki`.
- **Grafana IRM's incidents carry no linked alerts, and the docs say
  why**: its incident API lists none, and its OnCall API's alert groups
  name no incident (docs/design/links.md, "Grafana IRM").
- **A step of an on-call feed's repeats is a period, or a month of a
  yearly one, and each day it makes, those `BYSETPOS` or a limiting part
  drops among them**, where 0.69 counted a week of a weekly repeat as
  one; the feed's 2,000,000 steps take its zones' too. A feed of many
  repeats whose days make no turn can now pass the bound and be refused,
  as 2,000 yearly repeats keeping one day in 366 (`BYSETPOS=-366`),
  340 KB, are.
- **A `TZID` that is IANA's name for a fixed offset** (`EST`, `MST`,
  `HST`, `Etc/GMT+5`) is read by the feed's own `VTIMEZONE` of that name,
  when it has one, where 0.69 read IANA's fixed offset: a feed's `EST`
  that keeps summer time was an hour off all summer.
- The chart refuses a `loki.tenant` or `rules.ruler.tenant` with `|`,
  `/`, `\` or a line break as it renders, where the pod refused it as it
  started.
- CI's and the release's tests are given forty minutes, where twenty
  left the serve package unfinished on a slow macOS runner.

### Fixed

- **A version of the rules added while serve read the history was taken
  for the rules changed then** (affected: 0.29.0 to 0.69.2). A look read
  the history's directory twice, and a version added in between was
  recorded from when it was seen; the hours since its own time were
  never replayed with it. The rules in effect are now read no later than
  the last version followed, and the next look follows it from its own
  time. It made `test/e2e/rendered.sh` fail about once in 470 runs.
- **A replica told to stop closed its ports at once and ended its
  requests** (affected: 0.36.0 to 0.69.2; requests were ended on every
  stop since 0.6.0). In a rolling update the Service still sent it
  connections for a moment, and they were refused; a request under way,
  a notification among them, was answered with nothing, a 200, as if
  done. serve now answers for `--shutdown-delay` first, not ready
  meanwhile, then lets the requests under way finish, ten seconds at
  most. A request whose context ended before it was done is answered
  503. It made `test/e2e/replicas.sh` fail about once in 60 runs.
- **A rule judged from Grafana's own alert state history had no severity
  in `lapilli serve`** (0.68.0 to 0.69.2): it has the one its labels
  say, as every other rule has. And a severity serve joined for rules
  of one name read one extra (`critical/critical/warning`, where verdict
  says `critical/warning`).
- **A change approved through a Mimir's, a Cortex's or a Loki's ruler
  is written only in the tenant it was proposed in** (affected: 0.23.0
  to 0.69.2, Mimir's and Cortex's) (security). serve started again on
  the same ruler with `--ruler-tenant` naming another tenant read the
  change again there, and wrote it whenever the rule hashed alike. Such
  a change is refused once approved now, and one sent but not yet read
  back is read back only in its own tenant.
- **An on-call feed's turn at a time the clocks skip or show twice**
  was read an hour off. A `DTSTART` is read as RFC 5545 (3.3.5) reads
  one: 02:30 in the gap is 03:30, where 0.69 made it 01:30; of two, the
  first, where 0.69 took the second east of UTC. A repeat's day the
  clocks skip is read so too, and kept and counted by `COUNT`: there
  Lapilli departs from RFC 5545 on purpose, whose 3.3.10 says such a day
  is ignored and not counted, as a turn dropped would leave no one on
  call.
- **An on-call zone's name over 256 bytes is refused**: a feed could
  keep 1,024 names of 64 KB each.

## [0.69.2] - 2026-11-23

What 0.68.1's reviews left: a rule Grafana's history can't judge is left
out alone, remembered, where its day was a gap for every rule; a pull
request's backtest takes the place of the one waiting by its repository's
id; the backtest gate takes a base from GitHub's own merge alone; and the
firing and tracker recipes hold their requests as the incident recipes
do. Each fix of code with a test that failed before it.

Before upgrading:
- **Ledger schemas 030 and 031**, after 029, in one start: keep a copy of
  the ledger's disk, or of its database, first
  ([Going back after a schema upgrade](charts/lapilli/README.md#going-back-after-a-schema-upgrade)).
  A lapilli before this one doesn't open the upgraded ledger.
- **A Grafana that keeps its alert state history in annotations, its list
  of alert rules** (`/api/v1/provisioning/alert-rules`, which a token
  that exports the rules reads too) **past 64 MB or 50,000 rules**, or
  past what a day's read of the history leaves of the reader's 256 MiB
  and 5,000 requests, has none of the rules Lapilli can't replay judged
  from that history, where 0.69.1 judged them by their titles: each day
  `lapilli serve` copies in for them is a gap, logged, and `lapilli
  verdict` lists them, saying why. The list is read once a day copied,
  where its history holds a change of a judged rule's title; with a
  history kept in Loki, never (Fixed (security)).
- **A backtest job on GitHub that checks the pull request's branch out by
  its name** (`ref: github.head_ref`), or any commit but the event's head
  or the commit GitHub's event names (`GITHUB_SHA`), GitHub's merge of
  it, with a gate or `STATUS`, fails where the checkout isn't the event's
  head: check out the default, or `ref:
  github.event.pull_request.head.sha`. `refs/pull/N/merge` checked out by
  its name in a run again after the base moved is GitHub's merge made
  again, not `GITHUB_SHA`, and is refused too (Changed).
- **The firing recipes take https alone**, or http to this machine, and a
  token file of one line; `put.sh` takes http or https (Fixed (security)).
- **The tracker recipes take their tracker's API over https alone**
  (`GITLAB_URL`, `JIRA_URL`, `AZURE_DEVOPS_URL`, `GITHUB_API`,
  `LINEAR_API`, `SHORTCUT_API`), and every token file of one line: a job
  whose tracker is reached over plain http stops, saying which setting.
  With the chart, the recipes are its own copies, upgraded with it
  (Fixed (security)).
- **A route whose `teams:` names a team in another letter case than
  either of two teams alike** takes neither's events (Changed).
- **A rule of a branch other than the default that requires
  `lapilli/backtest`** needs `lapilli/backtest (into BRANCH)` now
  (`lapilli/backtest/KEY (into BRANCH)` with a `KEY`), on GitHub, Gitea
  and Forgejo Actions, and GitLab, where the CI says the default branch,
  in place of `lapilli/backtest`: a rule that keeps it, beside the new
  name or alone, is met by the same head's pull request into the default
  branch, judged against the default. A rule of a pattern (`release/*`)
  asks one name of every branch it matches, none of them each branch's
  own: give each such branch a rule of its own, naming its status in
  full, and one to each branch made later. Require each status by its name
  in full, never by a pattern of names (Gitea's and Forgejo's
  `lapilli/backtest*`), which takes the names a pull request's author
  makes by the branch they open it into. The default branch's is named as
  before (Changed).
- **A backtest job whose `TARGET` is a short name a tag has too**
  (`origin/main` beside a tag `origin/main`, or a short hash a tag is
  named as), with a gate or `STATUS`, fails: give the branch in full
  (`refs/remotes/origin/main`) or the commit's full hash (Changed).
- **A backtest job in GitHub Actions on an event of no pull request**
  (`push`, `workflow_run`) that gives `PR`, with a gate or `STATUS`, fails
  where `TARGET` is left at `HEAD^1`: give `TARGET` or `TARGET_BRANCH`,
  the branch the pull request goes into. With `STATUS`, where the event
  says the repository's default branch, as GitHub's do, and in Gitea's
  `push` and GitLab's branch pipelines likewise, give `TARGET_BRANCH`: the
  status's name says that branch (`lapilli/backtest (into BRANCH)` of one
  but the default), and a job that gives `TARGET` instead, or neither,
  sets no status, the log saying why (Changed).
- **The incidents' `put.sh` takes `http://` or `https://` alone**, an
  address with no scheme (`lapilli:8082`) or another refused, and a token
  file of one line, with no tab or other control character (Fixed
  (security)).
- **An address with `@` anywhere, in its query too**
  (`?owner=ana@example.com`), is refused by the firing, incident and
  tracker recipes, and by both `put.sh`s (Fixed (security)).
- **An incident tool or a tracker reached over plain http inside a
  cluster** needs `PLAIN_HTTP=true` now (in the chart,
  `serve.trackers.env`): without it, an `http://` address to a host but
  this machine is refused, and the job stops, saying which setting
  (Changed).
- **A backtest job that writes `TARGET=HEAD^1`**, the default, on Gitea's
  or Forgejo's Actions, GitLab, Bitbucket Pipelines or Azure Pipelines, is
  compared with the branch the pull request goes into, as one that leaves
  `TARGET` out is, not with `HEAD`'s first parent: that branch is the
  clone's, or fetched from `origin`, which needs the checkout's
  credentials kept. On Gitea's and Forgejo's Actions, `HEAD` a merge where
  the CI says no branch it goes into (`GITHUB_BASE_REF`), with a gate or
  `STATUS`, fails: give `TARGET` or `TARGET_BRANCH` (Changed).

### Fixed (security)

- **One Grafana rule of a judged rule's title stopped the record of every
  rule judged from Grafana's history** (affected: 0.68.1 to 0.69.1; 0.68.0
  took the one rule's firings away instead, which 0.68.1 fixed by this).
  The history names a rule by its title alone, and 0.68.1 left each day a
  gap for every rule judged from it while any of them shared its title, as
  the record was kept for the source and not for each rule: anyone who
  could make a rule in a folder Lapilli reads, of one judged rule's title,
  paused or not, or two teams by chance, stopped every team's. So did one
  rule the history passed over changes of, or one with a firing a write
  can't hold. Such a rule is left out of the day alone now, and the other
  rules' day is written: nothing of it is written or taken away, an alert
  of it on stays on, the log names it, and the period is kept for it
  (**ledger schema 030**, `left_out`: by source and rule, 64 periods a
  rule and 5,000 rules at most, dropped with `--keep`). Over a period it
  was left out a rule has no record, not a quiet one, wherever what the
  copies cover of it is read: its page, which says since when it is left
  out while it is; whether its numbers are set beside the span before;
  whether it has twelve weeks to be retired by; and what a rule only
  copied in is held to. The period stays so once the rule is judged again,
  in a round that reads it again within `--settle`, and after a restart:
  what the history says fired under the title meanwhile is no rule's.
  Judged again, the rule is read back from where it was left out, a week
  at most, and an alert on then is written from there; past a week, an
  alert of it on since before it was left out, that the read says nothing
  of, is ended where it was left out. A read back of no use where the
  day's read was (past any of the reader's bounds, Grafana's list of its
  rules refused or past its own, answers that can't be used) leaves the
  rules judged again out once more, not the day. Past 5,000 rules, the
  periods of rules no longer among Grafana's are let go of, those that
  ended earliest first: the record of such a rule, kept under its old name
  once it is renamed or deleted, then reads as copied over them. And a
  change of Grafana's history is taken for a judged rule only where the
  history says it is that rule's: kept in Loki, by the rule's uid; in
  annotations, by its id (`alertId`), the one Grafana's provisioning API
  lists the rule's uid with (`/api/v1/provisioning/alert-rules`, which a
  token that exports the rules reads too), read once a day copied, within
  the reader's bounds. A rule it lists no more, with an id it lists
  another rule with too, or under another title, renamed since the rules
  were read, is left out. A title shared across folders leaves neither
  rule out, however many rules share titles; a rule of a judged rule's
  title in a folder Lapilli's token doesn't read takes nothing, its
  changes read or passed over (a change the read passes over leaves out
  the rule it says it is of alone); and a rule of a judged rule's title
  made, firing and deleted between two rounds, which 0.68.0 to 0.69.1 took
  for the judged rule, its firings written into the judged rule's record
  and its alert ending the judged rule's, writes nothing. Where the
  history says no rule's uid or id (a Loki line without one, as whoever
  can push to the tenant may write; Grafana's annotations of alerts always
  carry one), a rule of a judged rule's title in any version of the rules
  in effect across what a round writes leaves the judged rule out alone,
  and shows as no record; one no read of the rules saw, in a folder the
  token doesn't read or there between two rounds, is still taken for it;
  and such a line passed over leaves out every rule of its title. A name
  the read holds no change of is judged, with no firing, whoever else has
  it. What still makes a day a gap for every rule judged so is of the
  whole read: the history refused or past the reader's bounds, changes
  passed over of more than 1,000 rules, in any folder, Grafana's list of
  its rules refused or past its own (64 MB, 50,000 rules) or what the
  day's read leaves of the reader's, more than 100,000 firings in a day,
  more than 5,000 rules to keep as left out, none of them gone from
  Grafana, or none of the rules judged
  ([docs/design/grafanajudged.md](docs/design/grafanajudged.md)).
- **The backtest gate could pass a pull request read from a branch its
  author moved** (affected: 0.32.0 to 0.69.1; the gates that stop a
  merge, and the status, from 0.67.0). 0.68.1 took `HEAD`'s first parent
  for the base only of GitHub's merge, told by `HEAD^2` the event's head.
  A merge the pull request's author pushes has that head for a parent
  too: a job that checks the branch out by its name (`ref:
  github.head_ref`), after the branch moved, read a merge of the event's
  head into a commit its author chose, one holding the change already,
  or the base's own commit under the base's files, took that commit for
  the base, found nothing changed, passed `FAIL_MISSED`, and set a status
  of success on the event's head, a commit it hadn't read. `github.sh`
  gives the merge GitHub made, a `pull_request` event's `GITHUB_SHA`
  where `GITHUB_REF` is `refs/pull/N/merge`, and `HEAD`'s first parent is
  the base only where `HEAD` is that commit, its second parent the
  event's head; otherwise the branch the pull request goes into is taken
  (`GITHUB_BASE_REF`), as of a checkout of the head. And where the event
  says the head, a `HEAD` that is neither it nor GitHub's merge of it (a
  merge its author made, a commit pushed after it) is refused with a gate
  (`FAIL_ON_ERROR`, `FAIL_ON_EXPECTATIONS`, `EXPECT_REQUIRED`,
  `FAIL_MISSED`) or `STATUS`, whatever `TARGET`, `TARGET_BRANCH` or
  `STATUS_SHA` say: the job fails, the comment saying why, and its
  status is the one failure, on the event's head, saying it was refused,
  with no pending before it, so that no earlier run's success, or the
  pending of one killed, stands there for this run (Changed). The default
  checkout, GitHub's merge, and a checkout of `pull_request.head.sha` are
  judged as in 0.69.0.
- **A firing recipe read its token file whole, and gave curl no scheme**
  (affected: 0.19.0 to 0.69.1). `google-cloud.sh` from 0.19.0 and `put.sh`
  from 0.20.0 sent what `GOOGLE_TOKEN_FILE` and `LAPILLI_TOKEN_FILE` hold
  as it was: a second line, or a carriage return, went into curl's
  headers as a header of its own. `put.sh` sent serve's token to an
  address of any scheme, one with a user and password before `@` too,
  and `google-cloud.sh` said the address it refused, a user and password
  with it. `datadog.sh` and `newrelic.sh` (0.67.0) took https, or this
  machine, and one line of a key's characters already; none gave curl
  the scheme. `datadog.sh`, `newrelic.sh` and `google-cloud.sh` take
  `https://` now, or this machine over http, for tests, refuse `@`, give
  curl that one scheme (`--proto`), and take a key or token file of one
  line of a bearer token's characters; `google-cloud.sh` holds what
  `gcloud auth print-access-token` writes the same. `put.sh`, which
  writes to serve's port as a Service in the cluster is reached, takes
  `http://` or `https://` and no other scheme, pinned, refuses `@`, and
  takes a token file of one line with no tab or other control character,
  serve's token being of any characters. Neither the token nor the
  address is said. `cloudwatch.sh`, `azure-monitor.sh` and `oci.sh` ask
  through their cloud's CLI, and hold no token and no address of their
  own.
- **A firing recipe read an answer of any size** (affected: 0.19.0 to
  0.69.1; Datadog's and New Relic's from 0.67.0). Each answer is 16 MiB
  at most now, whatever curl's version: `--max-filesize`, which before
  curl 8.4.0 holds only an answer that says its length first, and what
  curl writes cut there too; past it, the script stops, writing nothing.
  Serve's answer to `put.sh`, its own, is held by `--max-filesize` alone.
- **The incidents' `put.sh` and the tracker recipes read their token
  files whole, and gave curl no scheme** (affected: `put.sh` 0.15.0 to
  0.69.1; `github.sh`, `gitlab.sh`, `jira.sh` and `linear.sh` from
  0.22.0, `azure-boards.sh` and `shortcut.sh` from 0.27.0, and the
  chart's copies of them). Each sent what its token file holds as it
  was, Lapilli's and the tracker's: a second line, or a carriage return,
  went into curl's headers as a header of its own. `put.sh` sent serve's
  token to an address of any scheme, and the tracker recipes their
  tracker's to whatever `GITHUB_API`, `GITLAB_URL`, `JIRA_URL`,
  `LINEAR_API`, `AZURE_DEVOPS_URL` or `SHORTCUT_API` said, plain http
  and a user before `@` too. They are held as the firing recipes are
  now. Lapilli's address (`put.sh`'s argument, `LAPILLI_URL`) is
  `http://` or `https://`, as serve's port is reached in its cluster,
  and a tracker's API `https://`, or this machine over http, for tests;
  one with `@` is refused; curl is given each address's scheme alone
  (`--proto`); and a token file is one line, what an editor leaves after
  it dropped: of a bearer token's characters for GitHub's, GitLab's and
  Shortcut's, of any but a tab or another control character for
  Lapilli's, Jira's, Azure DevOps' and Linear's. Neither a token nor an
  address is said.
- **A tracker recipe read an answer of any size** (affected: 0.22.0 to
  0.69.1). Each answer, Lapilli's or the tracker's, is 16 MiB at most now,
  whatever curl's version: `--max-filesize`, and what curl writes cut
  there too; past it the task is skipped, or the run stops, as with any
  answer that fails. A request is asked again by the recipe, not by
  `curl --retry`, which writes each try's answer after the one before
  when the answer goes through a pipe, a read that timed out midway
  followed by the next try's whole: the tracker's and Lapilli's alike,
  three times at most, each try's answer the last alone, after a second,
  then twice as long, or as long as `Retry-After` says, a minute at most;
  a write that makes something only where it can't have been taken
  (below). Serve's answer to the incidents' `put.sh` isn't kept.
- **A task made of a proposal mentioned an issue of its tracker's**
  (affected: 0.22.0 to 0.69.1). Its title was `Proposal #12: ...`, which
  a tracker recipe makes its issue's title, where `#12` is the host's own
  issue or pull request 12, mentioned by it. It is `Proposal 12: ...`
  now, as a change's pull request says Lapilli's own numbers since
  0.68.1. A task made before keeps its title; it is its proposal's by the
  proposal's task id, and its issue is found by the marker in the
  issue's text, neither by the title.
- **A route's `teams:` took another team's events, where two teams'
  names differ only in letter case** (affected: 0.64.0 to 0.69.1). A
  route matched a team's name in any letter case. A ledger from before
  0.44.0 may hold two such teams (`Payments`, `payments`), which the
  Teams page lists apart, to be renamed: a route naming one sent the
  other's tasks, proposals and goals to its destinations too. A route
  names a team as it is written now, or in another letter case where one
  team alone has the name, as `--config`'s own teams are matched: of two
  alike it takes the one it names, and one naming neither as written
  takes neither's, which go to `default` (Changed). `teams: [Payments]`
  still takes the one team `payments`.
- **Looking at the Sources page started a source's clock** (affected:
  0.64.0 to 0.69.1). A source whose reads aren't counted (the incidents
  file, an impact signal, an on-call schedule), never read well, is down
  once it has failed past its bound, counted from when the replica first
  saw it failing: noted as the page, or `GET /api/v1/sources?all=true`,
  judged it, a viewer's request too, in the replica's memory, a time for
  each source serve lists and no more, nothing in the ledger. The first
  failed read is noted where the source is read now, and judging a source
  reads it and writes nothing; the leader says it with its status, so a
  replica that follows judges by the leader's reads, not by when its own
  page was first looked at. And a replica that led again judged by what
  it read in a term it led earlier, kept while another led, which may
  have read the sources well all along: days on, a source read well then
  was quiet and down before this term read it, and down at once at its
  first failed read, which a notice could tell (`source.down`). A replica
  that starts to lead takes the sources' reads of the term before now,
  before its heartbeat says anything of the term: the leader's, as it
  last said them, which a follower keeps from the leader's heartbeats,
  where it said them within a lease, a campaign and three beats of the
  handover (a leader whose heartbeat stopped writing while its lease was
  still renewed passes none on); its own, where it led that term; or
  else none, each source waiting until it is read. So a source failing
  is failing since when the leader before said, and one that never reads
  well is down past its bound however often the leader changes.
- **The backtest gate read a `TARGET` of a branch's name as a tag of
  that name** (affected: 0.32.0 to 0.69.1; the gates that stop a merge,
  and the status, from 0.67.0). A `TARGET` given by a short name, such as
  `origin/main`, was whatever git resolves the name to, and git takes
  `refs/tags/NAME` before `refs/remotes/NAME`: whoever could push a tag
  named `origin/main`, at a commit of theirs holding the change already,
  fetched with the history, had the gate compare the pull request against
  that commit, pass `FAIL_MISSED`, and set a success on the event's head.
  A branch's name is that branch in full now, `refs/remotes/NAME` before
  `refs/heads/NAME`; a name a tag has too, a short hash among them, is
  refused, failing the job with a gate or `STATUS`, its status the one
  failure, or said in the comment without; a full ref or a full hash is
  taken as it is (Changed). `TARGET_BRANCH`, and what each host's script
  gives from its CI, are full refs or full hashes already.
- **The same head, opened as a second pull request into a branch its
  author chose, replaced the failure with a success** (affected: 0.67.0
  to 0.69.1). A status is a commit's and its name's, not a pull
  request's: the pull request's author could open the same head again
  into a branch of theirs holding the change already, whose backtest
  against that branch passed, and its success took the place of the
  failure the pull request into the default branch left, the status that
  branch's rule requires. Where the CI says the repository's default
  branch, GitHub's and Gitea's event (`repository.default_branch`) and
  GitLab's `CI_DEFAULT_BRANCH`, a pull request into another branch sets
  `lapilli/backtest (into BRANCH)` now, and the default's stays as its own
  pull request left it; a branch whose name holds more than letters,
  digits and `. _ / -`, or more than 200, sets none (Changed). In an event
  of no pull request (GitHub's `push` or `workflow_run`, Gitea's `push`,
  GitLab's branch pipeline), the CI says the default branch but not the
  one the pull request goes into, and the status kept the default's name
  whatever `TARGET_BRANCH` said: a job that judged the same head against
  the branch of a second pull request, `TARGET_BRANCH` taken from it, set
  `lapilli/backtest` success over the failure the job into the default
  branch left. `TARGET_BRANCH` is the branch there, named as the CI's
  would be; a `TARGET`, or none, names no branch, and no status is set,
  the log saying why (Changed). Bitbucket Pipelines doesn't say the
  default branch, and its build status is still one of a commit and a
  key, the last pull request's standing for both; Azure DevOps' status is
  the pull request's own. The docs' workflow runs on pull requests into
  the protected branches alone (`branches:`), and say to require a
  branch's own status by its name in full: not `lapilli/backtest` beside
  it, nor one rule of a pattern (`release/*`) for several branches, nor a
  pattern of names (`lapilli/backtest*`).
- **The backtest gate took a merge for GitHub's in an event of no pull
  request, and one of another head for the event's** (affected: 0.67.0
  to 0.69.1). In GitHub Actions, a `push` or `workflow_run` job that gives
  `PR` had `HEAD` a merge taken for the CI's, as outside a CI, and its
  first parent for the base: a merge of the change into a commit its
  author chose passed `FAIL_MISSED` and got a success, where the same
  change as a plain commit was refused. GitHub says the merge it makes,
  so one it doesn't say isn't taken: `TARGET` `HEAD^1` is refused there
  with a gate or `STATUS`, unless `TARGET` or `TARGET_BRANCH` says the
  base; so in Gitea's and Forgejo's Actions, which make none. And a
  `GITHUB_SHA` whose second parent isn't the event's head, a merge of an
  earlier push beside the later head, was read, compared with
  `GITHUB_BASE_REF`, and passed with no status: it is refused, its
  failure on the event's head. A merge queue's run (`merge_group`) is
  said plainly not to be taken yet, and fails, as it did.
- **An earlier run's success or pending stood where a backtest with
  `STATUS` stopped before it set the status** (affected: 0.67.0 to
  0.69.1). A run refused where the CI says no head and `STATUS_SHA`
  isn't given, a path of `PATHS` in neither commit, a setting found wrong,
  and GitHub's merge in a clone of depth 1, whose parents the clone
  hasn't, each failed the job and set nothing. Each sets the one failure
  now, on the head the CI says, or `STATUS_SHA`, or else `HEAD` where it
  is a commit of one parent; a setting read before the status's own (`PR`,
  the host's API, `KEY`, `STATUS`, `STATUS_SHA`, `STATUS_URL`) can't set
  one.
- **An incident recipe read a redirect as a page of no incidents, and
  `google-cloud.sh` as one of no alerts** (affected: `google-cloud.sh`
  0.19.0 to 0.69.1; `pagerduty.sh`, `incident-io.sh`, `grafana-irm.sh`
  and `compass.sh` to 0.69.1). They asked with `curl -f`, which fails an
  answer of 400 or more alone, and doesn't follow a redirect with the
  token: a 307 was read as an empty page, and the script exited 0,
  writing that nothing fired, or no incidents, which `put.sh` copies to
  serve as the tool's. An answer that isn't a 2xx stops each now,
  writing nothing.
- **An incident recipe read an answer of any size, with a curl before
  8.4** (affected: 0.15.0 to 0.69.1). 0.68.1 held an answer to 16 MiB by
  `--max-filesize`, which before curl 8.4.0 (Ubuntu 22.04 has 7.81) holds
  only one that says its length first: a chunked one was read whole. What
  curl writes is cut at 16 MiB too now, whatever its version; past it the
  script stops, writing nothing.
- **The backtest gate took the first parent of the author's own merge for
  the base where a job wrote `TARGET=HEAD^1`** (affected: 0.32.0 to
  0.69.1; the gates that stop a merge, and the status, from 0.67.0). The
  scripts of the CIs that say the base, Gitea's and Forgejo's Actions,
  GitLab's merge requests, Bitbucket's and Azure's pull requests, set
  their default, the branch the pull request goes into, only where
  `TARGET` wasn't given, while lib.sh takes `HEAD^1`, its own default,
  written out as none given. A job that wrote it, of a head that is a
  merge its author pushed, its first parent a commit of theirs holding
  the change already, compared the pull request against that commit,
  passed `FAIL_MISSED`, and set a success on the head. `HEAD^1` written
  out is the default now, in each script. And Gitea's and Forgejo's
  Actions, which make no merge in any event, said so only of an event of
  no pull request: `HEAD` a merge is the author's in every event now, and
  the branch the pull request goes into (`GITHUB_BASE_REF`) is taken, or
  else, with a gate or `STATUS`, the run is refused (Changed).

### Fixed

- **A Grafana rule named apart from another rule of its name was judged
  as one that never fired** (affected: 0.68.0 to 0.69.1). The history
  names a rule by its title, and serve read it by the name the rule is
  judged under: for a rule told apart from a Loki ruler's or a cloud's
  alarm of its name, `Orders slow [Ops/shop]`, a name the history never
  has, so its days read as ones with a record and nothing fired. It is
  read by its title now, and written under the name it is judged by.
- **A firing of Grafana's history on in a span the ledger let go stayed
  on** (affected: 0.68.0 to 0.69.1). The ledger keeps 64 spans of what a
  source's copies cover, the earliest let go past them. A firing that
  started in one let go, which the history then said ended, was written
  again only within the spans kept: it stayed on, and was written a
  second time from where the copies covered again. It is ended where it
  is, in a write of the second it started in that isn't taken as covered
  again; the span stays let go.
- **An alert on since a gap in Grafana's history could be taken away, or
  left on, by a later round** (affected: 0.68.0 to 0.69.1). One written
  from where the copies covered again was taken away by a round that
  began within `--settle` of there and read from after the alert began;
  one that ended and fired again in the gap, still on, wasn't written;
  one written from where the copies covered again that ended before the
  round after read was taken away by that round, which read from after
  it began, and the period read as quiet, with `--every 1h` and
  `--settle 1h`; and one written from a gap's end, which ended in a later
  gap, stayed on. An alert a read holds no change of stays as it is, one
  it says ended is ended from where it was written, one the ledger has
  from within the write, on or ended, is written from where the ledger
  has it, and one on at the first day after a gap is written from there
  whatever the ledger had on.
- **A pull request's backtest takes the place of the one waiting by its
  repository's id** (affected: 0.61.0 to 0.69.1; **ledger schema 031**,
  `backtests.repository_id`). The one waiting was matched by the entry,
  the repository's name and the pull request's number: a repository
  renamed while one waited didn't take its own one's place, another
  repository the entry pins, made under the old name, did, and from
  0.63.2 a repository whose name isn't kept (past 200 bytes, or with what
  doesn't show) took none's. It is matched by the repository's id now,
  which the ID token says and the entry pins: two repositories of one
  entry never take each other's place. A backtest asked before the
  upgrade, kept with no id, is matched by its name as before; a plain
  token's, whose pull request is the job's word, takes none's, as
  before.
- **An impact window confirmed, reopened and confirmed again wrote a new
  incident each time** (affected: 0.6.0 to 0.69.1; `POST
  /api/v1/incidents/{id}/decide` from 0.64.0). Each was of the same
  hours, and each counted. A window confirmed again is the incident it
  wrote before now, the latest the ledger keeps, as the changes note it
  (`impact window KEY confirmed as ID`), while that incident stands for
  it: kept as a person's, no note since saying it was deleted, and its
  hours meeting the window's. It is given the hours and labels the window
  has grown to since, earlier or later, never fewer, and what they touch
  is judged again: a window confirmed while the impact went on had its
  firings after the hours first written black. Grown past what a
  person's change of it is held to, hours past 31 days or labels past
  4 KB, the confirmation is refused, naming the incident and saying why
  (`incident L-3 would grow past its bounds: an incident lasts 31 days at
  most`), and the incident and the window are left as they were. A title
  given is its title; none, or the window's own as its form sends it
  back, leaves the incident's. One deleted since and written again under
  its id, by anyone, of other hours or the same, or moved by a person to
  hours the window hasn't, is another's, and the window is written down
  anew; so is one confirmed longer ago than `--keep` keeps the changes. A
  verdict naming both the window and that incident names the incident
  once after it.
- **A tracker recipe made an issue or a comment twice where the tracker
  took the request and answered it with an error, or not in time**
  (affected: 0.22.0 to 0.69.1; comments from 0.27.0). A request answered
  500, 502, 503 or 504, or not answered within 30 seconds, was asked
  again three times at most whatever its method, as `curl --retry 3`
  does: an issue the tracker made, behind a proxy that answered 502, was
  made a second time, and the task linked to the second; so was a comment
  posted to an issue, and a comment copied to a task was sent to Lapilli
  again, which keeps it once. A write that makes something (an issue, a
  comment, a transition) is asked again now only where it can't have
  been taken: a connection refused, a 408 or a 429. A 503 isn't among
  them, as a proxy (Envoy) answers it after the tracker has the request.
  Otherwise the task is skipped for the run, saying so, and the next run
  finds the issue or the comment made by its marker. Each request is
  marked a read or a write where the recipe asks it, not by its method:
  a read, Linear's queries and Azure Boards' search among them, which go
  as POSTs, and a write a second try leaves as the first did (a state, a
  link), are asked again as before, and on a connection refused too.

### Changed

- **The count of firings that paged no one reads 2,000,000 of a span's
  firings at most** (affected: 0.68.0 to 0.69.1, which read them all),
  in the digest, a team's, and a report. It read every firing of the span
  without its labels in one statement, however many the span held. They
  are read in the order they started, 20,000 a statement; past
  2,000,000 the rest aren't read, are in none of the numbers, and
  `paged_no_one.partial` says so, as it does past 200,000 rows of mutes.
  Those of the second it is cut in, read before the cut, are counted
  whether their rule has mutes or not. The line then reads "The firings
  or their mutes were too many to look through: some firings weren't
  counted."
- **A backtest job on GitHub whose checkout is neither the event's head
  nor GitHub's merge of it fails with a gate or `STATUS`.** The branch
  checked out by its name, where it moved since the event; a merge ref
  checked out in an event whose `GITHUB_REF` isn't `refs/pull/N/merge`
  (`pull_request_target`, which the README says never to run this on);
  or a commit the job made itself. Check out the default (the merge) or
  `ref: github.event.pull_request.head.sha`. Without a gate or `STATUS`
  the comment is posted as before, compared with `GITHUB_BASE_REF` where
  `TARGET` is `HEAD`'s first parent. `github.sh` run outside GitHub
  Actions with an event file (`GITHUB_EVENT_PATH`) and a merge checked
  out gives `GITHUB_SHA` and `GITHUB_REF` too, or checks the head out.
- **A backtest refused sets one status, a failure, and no pending.**
  0.69.0 sets the status pending once the commits are found; a run the
  gate refuses finds none, and ends with the failure alone: of a
  `TARGET` that is the pull request's own commit, or a tag's name, on the
  head the CI says, `STATUS_SHA`, or `HEAD` where the CI says none, as in
  0.68.1; of a checkout that isn't the event's, on the event's head,
  whatever `STATUS_SHA` says, never on the commit checked out, which
  another event's run judges. A run that stops otherwise before the
  status is set (`PATHS` in neither commit, a setting wrong, a clone
  without the merge's parents) sets the failure too; a failure whose
  commit the backtest couldn't be shown to have read goes on the head the
  CI says, or `STATUS_SHA`, never on the commit the pull request goes
  into.
- **The tracker recipes take a tracker's API over https alone**, or this
  machine over http, or, with `PLAIN_HTTP=true`, http to any host: a
  self-managed GitLab, Jira or Azure DevOps Server reached over plain http
  is refused without it, as an incident tool is. Their token files, and
  the incidents' `put.sh`'s, may end in a carriage return or spaces.
- **The firing recipes take an address's scheme in capitals**
  (`HTTPS://`), `http://[::1]`, and this machine with no port, any other
  host over http still refused unless `PLAIN_HTTP=true`, and a key or
  token file ending in a carriage return or spaces, which `datadog.sh`
  and `newrelic.sh` refused. An answer is held to 16 MiB whatever curl's
  version, as the incident recipes' and the trackers' are.
- **`put.sh` of the firing recipes takes `http://` or `https://`
  alone**, and a token file of one line: a token with a space in it is
  sent as before, one with a tab or another control character refused.
- **A route naming neither of two teams alike as it is written takes
  neither's events.** With `Payments` and `payments` both kept, `teams:
  [PAYMENTS]` took both; it takes none now, and they go to `default`:
  name the team as it is written, or rename one, as the Teams page says.
- **A window confirmed again gives its incident the window's hours and
  labels as they are now**, and `title` in `POST
  /api/v1/incidents/{id}/decide`, or the form's, when one is given that
  isn't the window's own: an incident a person changed since keeps what
  they changed, and gains what the window has more, within what a
  person's change is held to; past it, the confirmation is refused, for
  the incident to be changed first.
- **A source never read well is down by its first failed read**, not by
  when a replica first looked at it: sooner by as long as no one looked,
  and no job judged it. A replica that follows a leader of an earlier
  release, which doesn't say it, shows such a source failing, and not
  down, until the leader is upgraded.
- **`PLAIN_HTTP=true` takes an API's address over plain http, to any
  host**, in the firing, incident and tracker recipes alike (default
  `false`; another value refused): for a self-managed tool or tracker
  reached inside a cluster, where https isn't served. Its token is read
  by whatever is on the way. curl is given http alone, an address with
  `@` is refused still, and the token checks stand. A refusal of plain
  http says so.

## [0.69.1] - 2026-11-23

The bar's team and period taken as they are picked, the sidebar's groups
told apart from their pages, and not known's sign in a ring. No schema
change: the ledger stays at 029.

### Changed

- **A team or a period picked in the bar is shown at once**: each is a
  link, the team's in a list that opens under its name, the period's (1,
  4 or 12 weeks) side by side, the one in effect marked. They were selects
  with a Show button to press after. The pages stay without a script; a
  pick keeps the page's other filters, up to 1 KiB of them, and the other
  choice as it was asked.
- **The sidebar's groups and their pages are told apart**: a group's name
  (Monitoring, Improve, Reporting, Administration) is a small dim label
  with a line above it, and its pages are in ink, a step in from it, on a
  guide line. They were dim alike, a size apart, and in Korean and
  Japanese, with no capitals, read as one list. The narrow screen's menu
  is drawn alike.
- Not known's sign leading a notice, a "?", is in a ring, so that it reads
  as a sign, as ✓, ⚠︎, ✕ and ↻ do, not as a question mark before the
  sentence.

## [0.69.0] - 2026-11-23

`lapilli demo`: a first report on a sample built in, with nothing to
connect. A pending commit status while a backtest job runs, so that a job
that dies never leaves an earlier run's success. And nine fixes left over
from 0.67 and 0.68, a flag that is on or off given `=false` among them.
No schema change: the ledger stays at 029.

Before upgrading:
- **A flag that is on or off takes `=true` or `=false`, and nothing
  else**: `=1`, `=t` or `=TRUE`, which `--check-replay`,
  `--check-content`, `--grafana-history`, `--progress`, serve's own and
  preflight's took, are refused. And `=false` turns one off where it
  turned it on (below): a command line that gave `--json=false` or the
  like now gets what it says.

### Added

- **`lapilli demo`: a first report, with nothing to connect.** It runs
  `lapilli verdict`'s own code over a sample built in: seven made-up
  rules of a small shop, a week of their history made up in the process,
  and three incidents. Its report shows a noisy rule and the longer `for`
  that would quiet it, the rules that caught an incident, one nothing
  caught and the rule that nearly did, and the firings that woke people;
  then how to run the same on your own rules. The same report every run:
  the sample's week, from Monday 9 November 2026, is laid in `--tz`'s
  zone, or the machine's. `--json` gives it as JSON. It reads no
  Prometheus and sends nothing anywhere (docs/design/demo.md).
- **A pending commit status while a backtest job runs** (`STATUS`, the
  action's `status`): once the recipe has found the commits, it sets the
  status pending, in each host's words (GitHub's, Gitea's and Azure
  DevOps' `pending`, GitLab's `running`, Bitbucket's `INPROGRESS`), then
  success or failure as before when the job ends. A job that dies never
  leaves a status that reads as success, an earlier run's on the same
  commit: ended by a signal (a CI's cancel) or an error of its own, it
  sets failure ("the job ended before the backtest did"); killed
  outright, it leaves pending, which a branch rule doesn't take as
  passing. A pending status the host refuses is said in the job's log,
  and the run goes on; a job that dies after sets failure all the same. Each host's status is tested against its
  reference's states with a server standing in, and Gitea's pending then
  failure against a real Gitea in CI (`test/e2e/backtest.sh`). No new
  setting; a job sets two statuses where it set one.

### Fixed

- **A flag that is on or off was on whatever it was given** (affected:
  0.1.0 to 0.68.1). `--json=false` was `--json`, in `lapilli verdict` and
  `lapilli impact suggest`, and so were `--linked-only=false`,
  `--redact=false`, `--alerts=false`, `--csv=false`, `--markdown=false`,
  `--metrics=false`, `--changed-only=false`, `--loaded-rules=false`,
  `--loki-loaded-rules=false`, `--fail-unreplayed=false`,
  `--fail-expectations=false`, `--expect-required=false` and
  `--fail-missed=false`, in serve too where it takes them. Each is off as
  `=false` says now, and `--redact-key` with `--redact=false` is refused.
- **Near misses were looked for by a rule's own labels, not its group's**
  (affected: 0.32.0 to 0.68.1). A rule whose group gave another service's
  label was asked about an incident it could never be about, and one whose
  group gave a label a signal's entry requires (`require:`) was passed
  over. A rule is looked at by the labels its firings carry now, its own
  over its group's, in `lapilli verdict` and `lapilli serve` alike.
- **vmalert's groups were replayed when vmalert started them, not when it
  asks their rules** (affected: 0.26.0 to 0.68.1). `--loaded-rules` placed
  a group in its interval by its `lastEvaluation`, as Prometheus's; vmalert
  starts a group at a random moment, but asks its rules, and stamps what
  fires, at a multiple of the interval, or `eval_offset` past one: a
  condition that held for less than an interval could fire in the replay
  where it didn't in vmalert, or the other way round. A rules API answer
  whose groups say their `concurrency`, as vmalert's alone do, through
  VictoriaMetrics' proxy too, is placed so now.
- **The Settings page said a new `--linked-only` before every firing was
  judged with it** (0.68.0 to 0.68.1). Started with it given or taken
  away, serve judges every firing again, once; until that is done, the
  pages show what was judged before, and the page now says so beside it,
  in every language, as `GET /api/v1/settings` does
  (`linked_only_judging`).
- **Replicas with different `--linked-only` judged every firing again at
  each change of leader** (0.68.0 to 0.68.1), each undoing the other, as
  in a rolling update that changes it. A leader judges everything again
  now only while the ledger's value is still the one it found at start:
  once for each change of value. A replica a newer change has passed
  judges what it runs with its own flag, as before, and nothing again;
  each says in its log, at start and as it leads, when its flag isn't
  what the ledger was last judged by, with both values.
- **A report from before the twelve weeks notifications are read for
  didn't say its notifications were counted from then** (affected: 0.22.0
  to 0.68.1; the firings that paged no one from 0.68.0). Its
  `notifications` section counted the notifications sent, and the firings
  that paged no one, from the twelve weeks' first Monday alone, and said
  them as the span's, or, of a span that ended before them, that none were
  taken. It says from when they count now, in the report's zone, and its
  numbers carry it as `counted_from`.
- **`jira.sh`'s issue search stopped at a 429** (affected: 0.15.0 to
  0.68.1), where its requests for Jira Service Management's alerts wait
  one out. It waits as they do now: as its `Retry-After` says, or else 5
  seconds and twice as long each time, five waits a request and ten
  minutes in all a run, theirs and its together. Any other answer that
  isn't a 2xx stops it, as before, now saying its status.
- The weekly digest read the notifications sent, and the firings that
  paged no one, twice (0.68.0 to 0.68.1): for its text and again for what
  the webhook is told. Once now, everyone's and each team's.

## [0.68.1] - 2026-11-21

A security release, after a review of 0.67.0 from an attacker's point of
view and one of 0.68.0's changes, each fix of code with a test that failed
before it. No schema change: the ledger stays at 029.

Before upgrading:
- **A backtest job that checks out the pull request's own commit**, not
  the CI's merge (its head a merge of its own too), with a gate
  (`FAIL_ON_ERROR`, `FAIL_ON_EXPECTATIONS`, `EXPECT_REQUIRED`,
  `FAIL_MISSED`) or `STATUS`, and `TARGET` left at `HEAD^1` or written as
  `HEAD`'s first parent otherwise, fails, refused, where its CI doesn't
  say the branch the pull request goes into: give `TARGET` or
  `TARGET_BRANCH`, or check out the merge. In GitHub Actions `github.sh`
  and the action take `GITHUB_BASE_REF`, which needs `fetch-depth: 0` or
  the branch fetched (Changed).
- **The incident recipes take https alone**, or http to this machine, and
  a token file of one line (Fixed (security)).
- **The chart's `linkedOnly`, `serve.everyoneAdmin` and
  `serve.exposeWithoutSignIn`** are `true` or `false`, quoted or not;
  anything else fails the install, naming it.

### Fixed (security)

- **A viewer could make serve hold memory without bound through the
  activity log's export** (affected: 0.64.0 to 0.68.0). `GET
  /api/v1/activity` gave a thousand changes unless told fewer, each
  change's before and after whole, indented, asked of the ledger all at
  once, and gave its page's turn back before it went out: a ledger's
  incident changed a thousand times, its labels and alerts, was an export
  of 58 MB that took 625 MB to work out, and sixteen readers who read
  nothing held 1.6 GB until the server's `WriteTimeout`, each holding its
  own. An export, and a page of the activity log, end now where their
  changes' text passes 8 MiB, as kept and, of an export, as written (JSON
  writes `<` as six bytes, CSV a quote as two), a change bigger than it
  given whole as the first of its page, and say where to go on from
  (`next`, `Link`), CSV and JSON alike; the ledger is read 200 changes at
  a time, a team's too, and a team's before and after 200 at a time; the
  JSON isn't indented; and an export holds a turn of the exports' own,
  four at once and one a person, until its reader has read it, 30 seconds
  at most, no page's. One of 50 changes at most takes a turn of the small
  ones', sixteen at once of all readers, and is held to a mebibyte.
- **The backtest gate could pass a pull request judged against its own
  commit** (affected: 0.32.0 to 0.68.0; the gates that stop a merge, and
  the status, from 0.67.0). `TARGET` is `HEAD^1` unless given, a merge's
  first parent, as GitHub checks a pull request out; where the job checks
  out the pull request's own commit (actions/checkout with `ref:
  pull_request.head.sha`), `HEAD^1` is the pull request's earlier commit,
  and `github.sh`, unlike the other hosts' scripts, took no branch from
  its CI: a pull request of two commits, a rule's change and anything,
  was compared against its first, nothing changed, `FAIL_MISSED` passed,
  a status of success was set on its head, and `INCIDENTS` was read from
  the pull request. `github.sh` gives the branch the pull request goes
  into (`GITHUB_BASE_REF`) and the event's head: `HEAD`'s first parent,
  however `TARGET` writes it (`HEAD^1`, `HEAD~1`, `HEAD^`, its hash in any
  case or short, `HASH^0`, `NAME~1`, as git resolves it; a branch's name
  alone is the one the job named), is taken only of GitHub's merge, `HEAD^2` the event's head, not of a head
  that is a merge of its own (an "Update branch"); otherwise the base
  branch is taken, and, with none, a gate or `STATUS` alone is refused and
  the job fails (Changed). Of a merge, the status goes on no commit the
  pull request goes into, nor the one it was compared against.
- **An agent's test file could make promtool load a hundred million
  values past `scenario.Bound`** (affected: 0.65.0 to 0.68.0). Bound read
  the file with yaml.v3, which keeps a mapping's own keys over a merge,
  and promtool reads it with yaml.v2, which lets a merge after them
  override them: `values: '1'` then `<<: {values: '0+0x100000000'}` was
  one value counted and a hundred million loaded, in serve's container,
  and the same took an `eval_time`, a series' line break and a tag past
  the checks. An agent's test file (`test_expectation`,
  `propose_expectation`, `propose`'s kind `add`) holds no merge, anchor,
  alias or tag now, but `!!str` on a text, as an added rule doesn't. An
  operator's test files, read from where `--expect` or `--config` is, or
  in a pull request's checkout, may, as before.
- **One Grafana rule of a judged rule's title took that rule's firings
  away, and left its days quiet** (affected: 0.68.0). The history names a
  rule by its title alone, and a title two of Grafana's rules have (one
  in two folders) is neither's: serve wrote the day without the judged
  rule's firings, so that anyone who could make a rule in a folder
  Lapilli reads, paused or not, or two teams by chance, took away its
  firings copied in within `--settle`, kept new ones from being written,
  and its days read as ones with a record and nothing fired; a firing
  that ended meanwhile stayed on. Such a day is of no use now, a gap, as
  one with a rule the history can't say all of is, logged with the rules'
  names; a rule judged so no more (paused, deleted, renamed) keeps what
  was copied in for it. A day is a gap for every rule judged from the
  history while any of them shares its title, as the record of Grafana's
  history is kept for the source and not each rule: name the rules
  apart, and the days after are written again.
- **The Sources page showed credentials its scrubber didn't read as one**
  (affected: 0.64.0 to 0.68.0). It took out a URL's user and password, a
  query's token or key, `name=value` between spaces, a JSON string, an
  `api-key:` header and a bearer or basic token of 12 characters or
  more: `password:`, `client_secret:` and `apikey:` as YAML and Go write
  a field, an Authorization header of another scheme (`GenieKey`, `SSWS`)
  or none, a shorter bearer token, a query's `pass` and `jwt`, or its `=`
  written `%3D`, a `Cookie:` header, a URL's password with an `@` or a
  `/` in it, a JSON string with an escape in it (a service account's
  `private_key`, its newlines written `\n`), a JSON number, a quoted
  libpq password, a Slack webhook's path, and a `ghp_`, `github_pat_`,
  `glpat-`, `xoxb-` or `sk-ant-` token were shown, to anyone who could
  read the page, an address to an admin; so were Grafana's service
  account (`glsa_`) and Grafana Cloud (`glc_`) tokens, Google's (`ya29.`)
  and a URL's password with a `/` and a `?` in it. Each is `***` now: an
  Authorization or Cookie header's value, or a password's or a key's,
  anywhere in a line (mid-text, after curl's `>`, after "header"), to the
  line's end or the sentence's, and a prefixed token at its real shape and
  length alone. An error that only names a credential reads as it was
  (`cannot fetch token: 400`, `invalid bearer`, and, its value a word of
  small letters in a sentence, `authorization: the token has expired`,
  `invalid password: check the file`), as do `glc_requests_total`,
  `ya29.png` and an address with an `@` in its path
  (`https://grafana.example/d/abc/team@ops`). Still shown: a value that
  starts with a word of small letters in a sentence (`cookie: abcdef`),
  and a password of digits and a `/` before a host with no port.
- **An incident recipe sent its token wherever its address pointed, and
  read its token file whole** (affected: 0.15.0 to 0.68.0).
  `pagerduty.sh`, `incident-io.sh` and `jira.sh` from 0.15.0,
  `rootly.sh`, `firehydrant.sh`, `grafana-irm.sh` and `compass.sh` from
  0.21.0, and Jira Service Management's `JSM_API` from 0.68.0 sent it
  over plain http, or to an address with a user and password before `@`,
  and a second line or a carriage return in the token file went into
  curl's headers as headers of their own. Each takes `https://` now, or
  this machine over http, for tests, refuses `@`, gives curl that one
  scheme (`--proto`), and takes a token file of one line: of a bearer
  token's characters where it goes in a header as it is, and of any but
  a space or a control character for Atlassian's, which go base64'd.
  Neither the token nor the address is said.
- **An incident recipe read an answer of any size** (affected: 0.15.0 to
  0.68.0). Each answer is 16 MiB at most now (`--max-filesize`, with curl
  8.4 or later for an answer that doesn't give its length); past it, the
  script stops, writing nothing.
- **A draft of what a rule is held to could be accepted by one no longer
  its approver** (affected: 0.65.0 to 0.68.0). The accept form's key is
  tied to the draft, its state and who saw the page, and the route asked
  only that its person approved some team's changes: one an approver of
  every team when the page was shown, now another team's approver alone,
  accepted it. It is asked as it is accepted now: an approver of every
  team, or of the one team that owns the rule.
- **An agent's rewrite in `test_expectation` reached promtool with
  templates an added rule is refused for** (affected: 0.65.0 to 0.68.0).
  Its labels and annotations, and its group's labels, which Prometheus
  gives each of the group's alerts, went into the group promtool runs a
  scenario with as the agent wrote them: a template that called a
  query, didn't parse, or grew without bound in serve's container. They
  are held as an added rule's are now (`templateOK`).
- **A rule's name in a change's pull request could close an issue once
  merged** (affected: 0.22.0 to 0.68.0; a draft's from 0.65.0). The
  change recipes make the title its pull request's title and its commit's
  first line, and the description the rest of the message, where a code
  span is no Markdown: a rule named `Fixes #12` closed issue 12, and
  mentioned it; `Fixes ABC-12` closed a Jira issue through GitLab's Jira
  integration, `ABC-1 #close` through Jira's smart commits, and `!12`
  mentioned a merge request. A word joiner (U+2060), which shows as
  nothing, goes between `#`, `!`, `GH-` or an issue's or a pull request's
  URL and its number, between a smart commit's `#` and its command, and
  inside a closing keyword before a reference or a Jira key, in the title
  and in each name or text from outside the description quotes; a keyword
  with no reference after it is left as it is, as Lapilli's own words
  are, and so is a Jira key alone, an incident's id as often (`INC-1`). Lapilli's own numbers are said without `#` (`Lapilli proposal 12`,
  `in Lapilli (draft 12)`), which mentioned the host's issue 12.
- **A cell of the activity log's CSV could be a formula after a space of
  Unicode's** (affected: 0.64.0 to 0.68.0). One that starts with `=`,
  `+`, `-` or `@` after spaces starts with `'`, which a spreadsheet shows
  as text; a no-break space (U+00A0), an ideographic one (U+3000) or
  another of Unicode's wasn't counted as one, and a spreadsheet that takes
  them off read a formula. They are now, and those of no width.
- **`lapilli impact suggest` and `lapilli verdict` printed a history URL's
  query** (affected: 0.1.0 to 0.68.0; `impact suggest` from 0.67.0). An
  error of a history that couldn't be reached quoted the request's URL
  whole, as text and in `--json`: a `--prometheus` URL with a token in its
  query (`?token=`) showed it. Each of the query's values is `[redacted]`
  now, and a password as net/url redacts one, in serve's errors too.
- **promtool could write a file past its volume** (affected: 0.65.0 to
  0.68.0). Its memory cap's wrapper set `RLIMIT_DATA` alone; promtool
  keeps its TSDB in its `TMPDIR`, the chart's `scenarios-tmp` volume of
  256 MiB. It sets `RLIMIT_FSIZE` to 256 MiB beside it now, on Linux: a
  write to one file past it fails (EFBIG), and the volume's `sizeLimit`
  bounds all of them.
- **The Cleanup page's delete took a rule out of its team without an
  admin** (affected: 0.64.0 to 0.68.0). While `--config` declares the
  boundaries of change, a rule's team is an admin's to move; deleting
  what the ledger keeps of a rule's name deleted its note, and the team
  it put the rule in, for an editor of every team. A note in a team is an
  admin's to delete there now.
- **The chart read `"false"` as true** (affected: 0.28.1 to 0.68.0). A
  template's `if` reads any text as true: `serve.everyoneAdmin: "false"`
  (from 0.28.1) made everyone who reached serve an admin,
  `serve.exposeWithoutSignIn: "false"` (from 0.30.0) let it be exposed
  with no sign-in, and `linkedOnly: "false"` (0.68.0) added
  `--linked-only`. Each is read as the bool it says now, quoted or not, in
  any letter case, and anything else fails, naming it.

### Fixed

- **A firing recipe passed a rule's name as the tool gave it, and serve
  refused the whole copy for one** (affected: 0.20.0 to 0.68.0; Datadog's
  and New Relic's from 0.67.0). A name with a character that doesn't show
  (Cc, Cf, Zl, Zp), or past 512 characters, stopped every firing of the
  period. The recipes take those characters out and cut the name to 512
  characters now, and a name with nothing left is named by its id
  (`monitor ID`, `condition ID`, an alarm's ARN or id).
- **`rootly.sh` had no end to its waits but `MAX_PAGES`** (affected:
  0.21.0 to 0.68.0). Each 429 was waited out in a shell of its own, so a
  run's waits were never added up. Each wait is two minutes at most now,
  five a request and ten minutes in all a run, and a `Retry-After` of `08`
  is read as eight.
- **`jira.sh` went round Jira Service Management's pages of alerts**
  (0.68.0). Two `startKey`s given in turn went on until
  `MAX_ALERT_REQUESTS`; a key given before stops it now, said, with
  nothing written.
- **A firing of Grafana's history ended after a gap was written over the
  gap** (0.68.0). It is written again from its start, and the write, from
  there to the day's end, covered every gap between, taken for days with a
  record, whichever round after them it ended in. It is written within
  the spans the copies cover now, each apart, and each gap stays one; an
  alert that began in a gap and stays on is written from where the copies
  cover again, in every round, as the first after the gap writes it.
- **The backtest recipes said a URL they take over http was refused for
  not being https** (0.32.0 to 0.68.0). `STATUS_URL`, the host's API,
  `LAPILLI_RELEASES` and `ACTIONS_ID_TOKEN_REQUEST_URL` take http to this
  machine too, and their messages say so.

### Changed

- **A backtest of the pull request's own commit is compared with the
  branch it goes into.** Where `TARGET` is `HEAD`'s first parent, given or
  not, however written, and `HEAD` isn't the CI's merge (`HEAD^2` the pull
  request's head as the CI says, or, where it says none, `HEAD` a merge),
  the branch the CI says the pull request goes into is `TARGET_BRANCH`
  (GitHub's `GITHUB_BASE_REF`; the other hosts' scripts gave theirs
  already); where it says none, a gate or `STATUS` is refused, and the job
  fails, saying to give `TARGET` or `TARGET_BRANCH` or to check out the
  merge. Without either, the comment is compared with `HEAD^1`, as before.
  A `TARGET` the job gives where the CI says the base (Gitea's Actions,
  GitLab's merge requests, Bitbucket's and Azure's pull requests) is
  taken as it is. A shallow clone's messages say `fetch-depth: 0`.
- **The incident recipes take an address's scheme in capitals**
  (`HTTPS://`), `http://[::1]`, and this machine with no port, any other
  host over http still refused, and a token file ending in a carriage
  return or spaces. Their README says to use curl 8.4 or later: earlier
  curl holds to the 16 MiB bound only an answer that gives its length.
- **The activity log's export, and its page, end at 8 MiB of their
  changes' text**, and say where to go on from, as at a thousand
  changes; its JSON isn't indented.
- **An agent's promtool test file is refused with a merge (`<<`), an
  anchor, an alias or a tag** but `!!str` on a text: write it out.
- A day of Grafana's history in which a judged rule's title is another
  rule's too is a gap, for every rule judged so, while it is: rename one,
  as the log says which.
- SECURITY.md and the docs say who writes the record of Grafana's rules
  judged from its history (whoever can push to its Loki tenant), that only
  Slack's markup is escaped in what serve posts (Mattermost, Rocket.Chat
  and Google Chat read their own), that a template's value in a link isn't
  URL-encoded unless the template says so, that a commit status is a
  commit's, not a pull request's, and that a team may add a rule to a
  place holding only another team's rules.

## [0.68.0] - 2026-11-21

Grafana's rules on data sources Lapilli can't replay (SQL,
Elasticsearch, InfluxDB, a cloud's monitoring, TestData) judged from
Grafana's own alert state history. How many firings paged no one, and
why, in the digest and the reports. Jira Service Management's and
FireHydrant's linked alerts, and near misses looked for by the link too.
No schema change: the ledger stays at 029.

Before upgrading:
- **The source `grafana` is `lapilli serve`'s own** from this release:
  serve copies Grafana's own alert state history in under it, and a job
  copying firings in under that name (`PUT /api/v1/firings/grafana`) is
  refused. A ledger where a job already has firings under it keeps them,
  and serve copies nothing under it until they are gone
  ([docs/upgrading.md](docs/upgrading.md#a-source-named-grafana)).

### Added

- **Grafana's rules Lapilli can't replay, judged from Grafana's own alert
  state history.** A rule `--grafana` holds whose condition reads a data
  source that is neither PromQL nor LogQL (SQL, Elasticsearch, InfluxDB, a
  cloud's monitoring, TestData) was only listed. Where Grafana keeps a
  history it serves (annotations, its default from 12.4 on, or Loki), its
  firings now come from that history, read as `--check-replay` reads it,
  within the same bounds, and are judged as copied-in firings are: stones,
  precision, re-firings, hours, incidents caught and missed, linked
  alerts. It stays not replayed, so no longer `for`, near misses or
  `--compare` for it; `lapilli verdict` says so under its table, and
  `--json` gives it `source: "grafana"`. `lapilli serve` copies the history
  in each round under the source `grafana`, a day a write, and ties each
  such rule to it by name, as a cloud's alarm is tied to its own: one rule,
  its page saying so. A day the history can't say all of (refused, too
  much, changes passed over, a firing a write can't hold, or behind its
  retention) is left a gap, never a quiet day. No new ledger schema. `--grafana-history=false` lists
  them as before (docs/design/grafanajudged.md).
- **How many firings paged no one, and why, in the digest and the
  reports.** Beside the notifications sent, the weekly digest, a team's,
  and a report's `notifications` section say how many of the span's
  firings no notification told of: muted by a silence, inhibited by
  another alert, muted by one or the other, or with no mute told of, as
  their pages say it; `digest.ready` and `report.ready` carry it as
  `notifications.paged_no_one`. Given once the ledger keeps a mute, and
  worked out when read: no schema change. Only rules with a mute about
  such a firing have their firings and mutes read, the hours about each
  firing's start and end, 200,000 rows of them at most; past that, the
  line says some weren't counted ([docs/design/mutes.md](docs/design/mutes.md)).
- **Jira Service Management's linked alerts** (`recipes/incidents/jira.sh`,
  with `JSM_CLOUD_ID`, the site's cloud id): each incident's alert ids from
  JSM's incident API, which Atlassian has in beta, and each alert from its
  operations API, as the same account. One that Alertmanager's Opsgenie
  notifier sent, the labels its group's alerts share in its extra
  properties (0.22.0 and later), is one of the incident's `alerts`. An
  issue the API says isn't an incident, and an alert it no longer has, are
  counted on stderr; a 429 is waited out as its `Retry-After` says.
  `ALERTS`, `ALERT_LABELS` and `MAX_ALERT_REQUESTS` as the other recipes
  take them.
- **FireHydrant's incident alerts** (`firehydrant.sh`): those `GET
  /v1/incidents/{incident_id}/alerts` lists, of the rule `alertname` names
  among their labels, or `alertmanager/labels-alertname`, as FireHydrant's
  Alertmanager source names it. `ALERTS`, `ALERT_LABELS` and
  `MAX_ALERT_REQUESTS` too.
- **`linkedOnly`** in the Helm chart passes `--linked-only`, to the
  CronJob and to serve alike (charts/lapilli/README.md).
- **The Settings page says whether `--linked-only` is in effect**, beside
  the judging settings, in every language, and `GET /api/v1/settings` as
  `linked_only`.

### Changed

- **Near misses are looked for by the link too** (docs/design/links.md).
  Of an incident that lists the alerts its tool linked to it, a rule it
  lists is looked at for a shorter `for` or a nearer number whatever
  `--match` says, nearly catching it where its labels agree with the
  alert's; with `--linked-only`, only the rules it lists are. One that
  lists none is looked at by labels, as before. In `lapilli verdict` and
  `lapilli serve` alike.
- **`datadog.sh`, `newrelic.sh` and `firehydrant.sh` wait out a 429**,
  as `rootly.sh` does: as Datadog's `X-RateLimit-Reset` or a `Retry-After`
  says (a wait written `08` is eight seconds), or else 5 seconds and
  twice as long each time; two minutes at most a wait, five waits a
  request and ten minutes in all a run, past which they stop with nothing
  written. Any other answer that isn't a 2xx stops them, as before, now
  saying its status.
- The incident recipes' `.jq` files take `ALERT_LABELS` as `--arg
  alert_labels`, not `--arg labels`, which `jira.jq` has for `JIRA_LABELS`;
  `jira.jq` and `firehydrant.jq`, run by hand, now want `jq -L
  recipes/incidents`, as the others do.
- The firings port refuses the source `grafana`, now `lapilli serve`'s
  own: a job copying firings in under that name names another.
- A rule judged from its history, a cloud's alarm tied to what is copied in
  for it among them, is counted in the report's table and in `--metrics`,
  and said under the table, where it was listed under "Could not replay".

### Fixed

- A cloud's alarm tied to its history by a copy read as replayed until the
  next run: the tie now carries the reason it isn't replayed, and its
  group, at once.

## [0.67.0] - 2026-11-21

Incidents judged by the alerts their tool linked to them, and why a
firing paged no one, from Alertmanager's event recorder. `lapilli impact
suggest` for a team with no incidents written down. Datadog's monitors and
New Relic's alert conditions judged from their history. A backtest that
can stop a merge, and set a commit status. Thanos and VictoriaMetrics
checked in CI. Ledger schema 028 and 029.

Before upgrading:
- **Ledger schema 028 and 029**, after 027: keep a copy of the ledger's
  disk, or of its database, first
  ([Going back after a schema upgrade](charts/lapilli/README.md#going-back-after-a-schema-upgrade)).
  A lapilli before this one doesn't open the upgraded ledger. Nothing is
  backfilled. 028 adds `incidents.alerts` and `catches.linked`; 029 holds
  `mutes`.

### Added

- **`lapilli impact suggest`** says which of the stacks in
  docs/impact.md's table (Istio, Linkerd, Envoy, ingress-nginx, Traefik,
  OpenTelemetry HTTP server metrics) the history has the metrics of, and
  for each found its series, the windows its `--impact` finds over the
  period with `--impact-for` 5m, and the `--impact '...'` to give
  `lapilli verdict`: for a team with no incidents written down. One
  request a stack counts its metric's series, and each found has its
  `--impact` read, a day a request. It takes verdict's flags for the
  history and the period, and `--json`; it only reads, and chooses none
  (docs/design/suggest.md). A test holds the table and the command's list
  alike. The table's "Envoy, Envoy Gateway" row is "Envoy or Envoy
  Gateway".
- **Datadog's monitors and New Relic's alert conditions, judged from
  their history** (`recipes/firings/datadog.sh`, `newrelic.sh`), as a
  cloud's alarms that aren't PromQL are: neither tool's rules are read or
  replayed. Datadog's from the events its monitors write (the Events API),
  each group from its Alert or Warn to its recovery; New Relic's from its
  `NrAiIncident` events (NRQL through NerdGraph), each incident from its
  open to its close. Each labels a firing with its tags, its priority as
  `severity` and the monitor's or condition's ID as `alarm_id`, bounded as
  `lapilli serve` takes labels; reads its keys from files and gives them
  to `curl` on stdin; stops past `MAX_PAGES`; and refuses an answer that
  may hold part of the history. `put.sh` copies them into serve. Checked
  against the answers each tool's documentation shows, not against real
  accounts.
- **`lapilli verdict --compare --fail-missed`** exits 1 when the change
  leaves an incident a rule caught before it caught by no rule after it,
  by `--incidents` or `--impact`, which it is refused without: a merge
  gate with no expectations file. The report says which incidents, and
  that the run fails for them (text, `--markdown`, `--json`'s `"missed"`),
  and the summary counts them (`missed_checked`, `incidents_missed`, new
  fields). With `--changed-only`, the rules left alone are replayed too
  when a rule that differs stops catching one, to see whether one still
  does. An incident a rule after the change that couldn't be replayed may
  still catch is not known (`missed_not_known`), said, and fails nothing.
  With `--fail-expectations`, either failing fails the run.
- **The backtest recipes stop a merge**: `FAIL_MISSED=true` gives lapilli
  `--fail-missed` and fails the job on it, once the comment says which
  (refused through `lapilli serve`, whose report doesn't say yet); and
  `STATUS=true` sets a commit status, `lapilli/backtest`, on the pull
  request's head commit, success or failure as the job ends, for a branch
  rule to require: GitHub's and Gitea's commit statuses, GitLab's,
  Bitbucket Cloud's build statuses and Azure DevOps' pull request
  statuses, on the iteration at that commit. The comment is still posted
  first. The action takes `fail-missed` and `status`; the token needs
  more for a status (docs/pr-backtests.md, "Stopping a merge").
- **Incidents judged by the alerts their tool linked to them**
  (docs/design/links.md). An incident takes `alerts`, each `{rule: NAME,
  labels: {...}}`, in the incidents file and in `POST` and `PUT
  /api/v1/incidents`: a firing of the rule whose labels agree with the
  alert's where both carry one (an external label it lacks passed over,
  as `--match` does) catches the incident when it starts between `--lead` before it
  and its end, whatever `--match` says. The report marks such a catch
  `linked` (`(linked)` in the text and in `--compare`'s Markdown, and in
  MCP's `caught_by`), and the incident's page and the rule's say "linked
  in the incident tool".
- **`--linked-only`**, in `lapilli verdict` and `lapilli serve`: an
  incident that lists alerts is caught by them alone; one that lists none
  is judged as before. serve judges every firing again once when it is
  given or taken away at a start.
- **The PagerDuty, incident.io and Rootly recipes write each incident's
  alerts**, those whose alert carries an `alertname` with its labels
  (PagerDuty's details as Alertmanager put them there, incident.io's
  attributes, Rootly's labels), saying how many they left out;
  `ALERT_LABELS` keeps some labels alone, `ALERTS=no` reads none, and
  `MAX_ALERT_REQUESTS` (2000) bounds what they ask for.
- **Why a firing paged no one.** `/v1/alertmanager/events` keeps the
  event recorder's mutes: when a silence or an inhibition kept an alert
  from a notification (`silenceMutedAlert`, `inhibitionMutedAlert`, and
  0.35's `mutedAlerts` of a notification), the alert's name and labels,
  and the silence's ID or the inhibiting alert's fingerprint; nothing else
  of a silence, not its comment or who made it. Both of the recorder's
  formats, 0.33 and 0.34's and 0.35's. Told of every few seconds, again
  after a failure and by each replica, a mute is kept once an hour, from
  the first to the last, and for `--keep`. A firing no notification told
  of says "muted by a silence" with its ID, "inhibited by another alert",
  or "muted by a silence or an inhibition", on the rule's page and the
  day's firings; MCP's `rule` gives each latest firing `muted`, read in
  the hours about the firing's start and end. Mutes come at the events'
  rate, 1,000 rows of a rule's hour and 200,000 a day at most, and claim
  only that a firing was muted ([docs/design/mutes.md](docs/design/mutes.md)).
- **Thanos and VictoriaMetrics, checked in CI.** A new job, `e2e-compat`
  (`test/e2e/compat.sh`), runs `lapilli verdict` and `--check-replay`
  against a real Thanos 0.42.5 (a sidecar and its query) and a real
  VictoriaMetrics 1.156.0 with vmalert 1.156.0, beside the Prometheus they
  read or are written by: the same firings, each its rule, labels, start
  and end, as against Prometheus; every firing alike with `ALERTS`,
  Prometheus's through Thanos and vmalert's in VictoriaMetrics; and
  `--loaded-rules` reading vmalert's rules through VictoriaMetrics. A pull
  request runs it when it changes `internal/prom`, `internal/replay`,
  `internal/rules`, `internal/source` or the script. Not a required check.

### Fixed

- **`--loaded-rules` reading vmalert's rules** takes each rule's
  `keep_firing_for`, which vmalert names so where Prometheus says
  `keepFiringFor`: it was read as none, so a vmalert rule's firing was
  replayed as resolving sooner than it did.

## [0.66.0] - 2026-11-21

The expectations' (0.65.0) follow-ups: what a review left as Low, fixed.
A CI job's scenarios capped as serve's are, a scenario whose test file a
change edited run, and the drafts, the rules added and the checks after
deploy each closer to what they say. No schema change: the ledger stays
at 027.

### Added

- **`lapilli verdict --promtool-memory SIZE`** caps each scenario a CI
  job runs, as serve does (256Mi unless given, 64Mi at least; on Linux,
  by RLIMIT_DATA; elsewhere a note that none is set). A promtool built
  with cgo maps 8 MiB more for each of its threads: give it 512Mi.

### Fixed

- **A scenario whose test file a change edited wasn't run** when its
  rule didn't change (0.65.0): `lapilli verdict --compare --expect
  --promtool` runs it before and after, a report of "nothing changed"
  shows the Expectations then, `--fail-expectations` holds it, and the
  backtest recipes gate and comment on it.
- **A catch or a scenario whose check moved to another week was told
  again** (`expectation.broken`) and, with `--expect-tasks`, made a second
  task while the first was open, the first's link gone from the rule's
  tab (0.65.0). The row moved keeps what was told of it and its task.
- **A rule only copied in was judged only inside its copies' latest
  span** (0.65.0): a week wholly inside any span the ledger keeps is
  judged; one touching a gap, or outside them, stays not known.
- **serve could run two promtools at once**, a round's scenario and an
  agent's check, each to its own memory cap (0.65.0): it runs one at a
  time; an agent's check waits within its own deadline, and is not known,
  saying so, past it. Neither holds the ledger's writer while it waits.
- **`test_expectation`'s `post` said a rewrite meets every expectation
  the rule is held to** when the agent gave the expectations itself
  (0.65.0): it says whose they are, those Lapilli holds the rule to or the
  agent's.
- **An accepted draft's pull request description could end a table cell
  early** (0.65.0), as 0.60.0's comments could: a backslash before a `|`
  in an incident's id is escaped as the comments' tables escape it.
- **An incident's id in expectations could span lines** (0.65.0), and
  look like another entry in a pull request's diff: it is one line of
  characters that show, a line break, control or format character refused
  in the file, `--config` and MCP, and left out of the history's draft;
  the Expectations tab sets it apart (`<bdi>`). A format character is
  refused wherever it is, a zero-width joiner in an emoji among them, and
  the whole file with it, as any line the strict reader refuses: check an
  expectations file's ids before upgrading.
- **Making room for a draft past the 1,000 kept could delete an accepted
  one** whose pull request was ready or open (0.65.0).
- **Why a check or a draft's item is not known showed in English** in
  every language (0.65.0), as did a scenario's passes and fails:
  Lapilli's words are the reader's language; promtool's own stay as said.
- **Why `--expect` can't be read showed on the status line to every
  reader** (0.65.0): to admins only, as `--config`'s problems are; others
  read that the file can't be read.
- **The change recipes refused `EXPECT_FILE` from a chart's ConfigMap**
  as "not the file serve read" when it differed by a trailing newline
  alone (0.65.0).
- **Two Grafana folders of one title** (nested folders) were one place
  for a rule added, their contact points combined (0.65.0): a group of a
  title another folder has too isn't offered, and one proposed before is
  refused as it is carried out.
- **An approved rule added was carried out** when an expectation it met
  as it was approved was not known by then (0.65.0), or when its group's
  labels had given it another team: both are refused as it is carried
  out.
- **A team's editor could propose a rule added but wasn't shown the
  form** (0.65.0).
- **A CloudWatch or Google Cloud alarm added whose `for` the cloud
  doesn't take** (not whole seconds; past CloudWatch's 24 hours) was
  refused only as it was carried out (0.65.0): it is refused as it is
  proposed.
- **A name of a rule added through an API could be proposed again in
  another place** before the rules were read again (0.65.0).
- **The new rule's form, MCP's `places` and a proposal adding a rule
  read the rules' version twice on each call** (0.65.0): once a version.
- serve's `--promtool-memory` help ended mid-sentence.

## [0.65.1] - 2026-11-21

The expectations' screens (0.65.0) checked as the others were: a budget's
week before the ledger's first firing has no number, and their Korean
words are the other pages'. No schema change: the ledger stays at 027.

### Changed

- Korean: the expectations' ledger is 기록, as on the other pages, a
  re-fire 재발화, as in the rules list, and a Latin name takes its
  particle after a space, as the other sentences' do.

### Fixed

- **A budget's week before the ledger's first firing read as none fired**
  (0.65.0): "? 0" on a rule's Expectations tab, `n: 0` in the API and
  MCP. It is not known with no number, "–", why `not_held`: no firing
  held isn't none fired. A week the ledger holds part of still counts
  what it holds.

## [0.65.0] - 2026-11-21

Expectations, steps 2 to 5 (docs/design/expectations.md): serve holds
each rule to what is expected of it after it is deployed; drafts of what a
rule is held to, from the history and from an agent; scenarios, promtool's
own test files, held to before a change is merged and after; and a new
rule, proposed held to its expectations before it is approved, added by
the four paths a change goes.

Before upgrading:
- **Ledger schema 027**, after 026: keep a copy of the ledger's disk, or
  of its database, first
  ([Going back after a schema upgrade](charts/lapilli/README.md#going-back-after-a-schema-upgrade)).
  A lapilli before this one doesn't open the upgraded ledger. Nothing is
  backfilled: the first round after the upgrade checks the thirteen weeks,
  and runs the scenarios, five a run. 027 holds `expectation_checks` and
  `expectation_drafts`, and adds `proposals.expect` and
  `proposals.expect_kept`.
- **Proposals and drafts share a person's hour**: the 20 proposals a
  person, with the agents acting for them, makes in an hour now count the
  drafts of expectations they make too.

### Added

- **serve's checks after deploy** (`--expect FILE`, the chart's
  `serve.expectations`; `expectations:` in `--config`; a rule's annotation
  `lapilli.dev/budget: "10/week, 2 asleep"`). After each run, the leader
  holds each rule to its budgets over the week so far and the twelve whole
  weeks before, by the ledger's weeks, the week so far broken as soon as
  it is over, and to its catches against the incidents written down,
  judged again by a replay of the rule as it is when a new version of it
  came after the incident (five a run). Not known is never shown as met or
  broken: a rule only copied in is known where its source's copies cover
  it without a gap, their latest span; a week before or past it, or a
  catch of an incident outside it, is not known.
  A check holds 10,000 rules and expectations, the expectations file's and
  `--config`'s first: a rule's annotation past what is left is not known,
  and never stops theirs. What it finds is kept (schema 027,
  `expectation_checks`) and shown on the rule's new Expectations tab, in
  the overview's What to look at (Make it a task, `POST
  /expectations/task`), on the rules list (`expect=broken`, `expect=held`,
  `expect=none`; `is:expected`, `is:unexpected`), in the digest
  (`expectations` in `digest.ready`), in `GET /api/v1/expectations`, MCP's
  `rule` (`expectations`) and `/metrics` (`lapilli_expectations_held`,
  `lapilli_expectations_broken`, `lapilli_rule_expectations_broken`). Told
  as `expectation.broken` once a rule, an expectation and a week, 50 a
  run; `--expect-tasks` (off by default) makes a task of kind tune for the
  rule's team, once a rule, an expectation and a week, 20 a run, none
  while one is open. Routes (0.64.0) take it, and the drafts' notices
  (`expectation.drafted`, `.accepted`, `.dropped`, `.pull_request`), by
  their rule's team and labels. The three sources combine as one file: `--expect` and
  `--config`'s are made one, the same budget or incident in both refused,
  the two kept as they last were; a rule's annotation holds it unless the
  file or `--config` give that rule, by `rule:`, the same budget, when it
  is passed over and said.
- **`labels:` and `refires` in expectations**, for `lapilli verdict
  --compare --expect` too: an expectation may name its rules by a label
  selector, as `--rules-select` takes, of the labels the rules' alerts
  carry, and a budget may bound re-fires (`refires`). Through serve's
  backtest port, while people slept is counted by the rule's team's own
  hours, as serve's checks count it.
- **Scenarios** (`scenarios:` in an expectation: paths of promtool test
  files, relative to the expectations file, inside its directory, 200 in a
  file, 1 MiB each). `lapilli verdict --compare --expect FILE --promtool
  PATH` runs each scenario of a rule the change touches, before and after,
  against the rule's group as each side has it: met, broken (what promtool
  said, in the report), or not known, and why (`not run: no --promtool`
  without it; a rule not in Prometheus's format; a test file that can't be
  read, a time out or a crash, which the summary counts as
  `scenarios_unrun`, as it counts one run before the change and not known
  after it, the change having moved the rule out of Prometheus's format).
  Counted with budgets and catches, so `--fail-expectations` covers them;
  a test file the change edited is said. `lapilli serve --promtool PATH`
  runs those of its own expectations (`--expect`, `--config`) after a run,
  again when the rule's group or its test file changes, five a run, never
  while holding the ledger's writer: kept in `expectation_checks` (with
  what promtool said, `detail`), and shown, told and made a task as a
  broken budget is. The chart's `serve.scenarios` puts the test files
  beside `serve.expectations`; `serve.promtool.image` copies promtool from
  an image of yours (Prometheus's own) by an init container, or
  `serve.promtool.path` names it in an image of your own. Lapilli's image
  has no promtool. The backtest recipes take `PROMTOOL` (the action's
  `promtool`), refused with `LAPILLI_SERVE`: serve runs no scenario a pull
  request sends, each said as not run through it. promtool runs with a
  fixed command line, no environment, 30s and 64 KiB of what it says at
  most, in a directory of its own, its `rule_files` the group Lapilli
  wrote, whatever the test file named, in a process group of its own
  killed whole at its time limit. On Linux, serve holds each run to
  `--promtool-memory` (256Mi unless given, 64Mi at least; the chart's
  `serve.promtool.memory`), promtool's RLIMIT_DATA, which lapilli sets by
  starting itself again before promtool: a rule's annotation template,
  which promtool expands at each evaluation of an alert that fires, could
  otherwise take gigabytes and the container with it. macOS sets no cap.
  serve keeps each run as begun before promtool starts, so that one that
  never ends (serve killed as promtool took its memory) isn't run again
  each term, only once promtool, the group or the test file changes; one
  serve stops itself, shutting down or no longer the leader, is run again.
- **Drafts from the history.** A rule's Expectations tab shows what it is
  held to as it is now: the most it fired in a whole week of the twelve
  before this one, while people slept and re-fired, rounded up (1 to 9 as
  they are, 10 to 99 to the next 5, from 100 to two significant figures,
  never below 1: a rule that never fired is held to 1), and the incidents
  it caught, within their delay rounded up to a minute. A team's rules',
  or every rule's, is a file to take (`GET /expectations/draft.yaml`,
  `team=`, `rule=`; `GET /api/v1/expectations/draft`), written by the YAML
  encoder, counted, never written by a model. `lapilli verdict
  --draft-expectations FILE` drafts one from the period it judged, by
  whole weeks back from its end. A person keeps the history's draft of a
  rule on its tab (Keep it as a draft) for another to accept.
- **MCP's `test_expectation`** (with `--mcp-compare`): checks an
  expectations text (a pull request's `expectations.yaml`) or serve's own,
  or a rewrite of the rule, against the rule replayed over the twelve
  weeks, keeping nothing: per item met, broken or not known, as it is and
  rewritten; an agent's scenario run by `--promtool`, its test file
  refused past 100 tests, 1,000 input series, 100,000 values (all promtool
  parses as values notation counted: `0+1x100000000` and each repetition
  among it, a native histogram as the numbers it holds each time it is
  repeated, an expected sample's histogram) or 10,000 evaluations a test,
  and refused where promtool would load what isn't counted (a `series`
  that isn't a metric name and its labels alone, a line break in `values`,
  a tag such as `!!binary`), before promtool runs, and run one at a time,
  within `--promtool-memory`. A rewrite that meets every expectation,
  losing no catch, the rule as it is and rewritten each replayed to the
  end, comes back with `post`, Markdown marked as an agent's rewrite
  backtested by Lapilli, with the numbers before and after, for the agent
  or a person to post on the pull request: Lapilli writes nothing there.
- **MCP's `propose_expectation`** (with `--mcp-propose`): an agent's draft
  of what one rule is held to, by `rule:` alone, checked as
  `test_expectation` checks it and kept for a person to accept or drop on
  the rule's tab, for the person the agent acts for, who can't accept it.
  No one accepts a draft they proposed, or their agent did; accepting is
  an approver's, in a browser, with `--changes`. Kept in
  `expectation_drafts` (schema 027): 10 waiting for a rule, 1,000 in all,
  the oldest decided let go. Told as `expectation.drafted`,
  `expectation.accepted`, `expectation.dropped` and
  `expectation.pull_request`, without a draft's text or reason. The open
  ones are listed on the proposals page; `GET /api/v1/expectation-drafts`.
- **A draft accepted goes the git path** where serve's expectations come
  from `--expect`, with `--changes-token-file`, and it holds no scenario:
  the file is read again and the draft taken into it, field by field, its
  comments kept, and handed to the change recipes
  (`GET /api/v1/changes`'s `expectations`,
  `PATCH /api/v1/changes/expectations/{id}`). The recipes take
  `EXPECT_FILE`, the expectations file in git `--expect` is made from, and
  write it alone, only while it is the file serve read, only as an
  expectations file. Otherwise accepting keeps the decision, and the tab
  gives the YAML to put where the expectations are declared.
- **A new rule, as a proposal held to its expectations** (kind `add`; the
  proposals page's Propose a new rule, `/proposals/new`; MCP's `propose`
  with kind `add`, and `places`). One Prometheus alerting rule, read
  strictly (one document; `alert`, `expr`, `for`, `keep_firing_for`,
  `labels`, `annotations`, each once, every value text; bounded) and
  written by Lapilli where it goes, never as given, named apart from every
  rule in effect; its labels plain text (a label's template would make
  each evaluation another alert, which the backtest wouldn't count), its
  annotations held as a fix to what a rule says is (templates that parse,
  no `query`, a runbook an https URL). It goes only where serve offers: a
  group of a rule file under `--rules` (either PrometheusRule's of a file
  of two), or a new group in a file one document of which holds groups
  (not a file a CI system runs); a Grafana folder's group, its query
  reading a data source the folder's rules read; a group of a Mimir's, a
  Cortex's or an Amazon namespace `--changes-in` names, or a new group;
  CloudWatch's or Google Cloud's `--alarms`, under a name `--changes-in`
  names there. It tells only whom the rules there already tell: in
  Grafana, a contact point of its group's rules (one of theirs where each
  of them names one; none where none does, Grafana's notification policies
  routing it by its labels as they route those); in CloudWatch, an SNS
  topic, an OpsItem or an Incident Manager response plan the alarms there
  take, never a Lambda function, an automation or an action that does
  more; in Google Cloud, a notification channel its policies tell. Added
  to a rule file or a ruler, it names no one: its labels route it through
  Alertmanager, as any rule's there do. It is held to one expectation at
  least, replayed over the twelve weeks with nothing beside it and checked
  as `test_expectation` checks one; no one approves it while one is broken
  or its backtest didn't replay it to the end (not known is shown, never
  met). Checked again as it is carried out (waiting for a comparison
  running, as a change does), it goes the way a change goes there: a pull
  request; Grafana's `POST`, with a uid of Lapilli's; the ruler's group
  written with it; CloudWatch's `PutMetricAlarm` or Google's
  `alertPolicies.create`. Undone, it is deleted. Once added, its
  expectations go where an accepted draft goes (origin `proposal`,
  accepted by its first approver); one merged by hand before anyone
  approved it has none kept, and holds up no other. An agent's counts
  toward its person's twenty an hour. It is the team's its name and the
  labels it is added with give it (its group's with its own), for a
  team's editor proposing it and for that team's approvers, freezes and
  `changes_in` (0.64.0). Not offered: Azure's rule groups and
  alert rules, OCI's alarms, a new Grafana group, a new rule file.
- **Grafana's `is_number`** in a rule's math expression is replayed.
- **lapilli preflight**'s `changes.aws` also asks, under each CloudWatch
  prefix `--changes-in` gives, the name of an alarm a rule added there
  would make.

## [0.64.1] - 2026-11-20

The Sources page on a laptop's screen, 0.64's sentences setting their
flags as code as the others do, and a rule judged by nothing said so on
its page and in the digest. No schema change: the ledger stays at 026.

### Changed

- **The Sources page fits a laptop's 1,280 px** in each language: a
  source's path or URL wraps in its row header, where it pushed the kinds
  into a letter a line in Korean and the table past the screen; its kind
  keeps its words and its last error breaks anywhere. Under 1,280 px it
  scrolls in its box, as the rules list does.
- A rule judged by nothing (`judged: false`) says so on its page's panel
  and its strip's days too, "N judged by nothing", as its firings and the
  rules list do (0.63.2).
- A refusal or a failure said on a page (a form refused, serve's status,
  a source's last read, a route's last error) sets a flag's name in it as
  code, as the pages' own sentences do; a flag's name in a URL's path or
  query stays text.
- A table's caption reads from the start, as the heading of what is under
  it, a little apart from the table before it (the routes' two tables, the
  Settings page's impact signals), and a row's own header is lined as its
  row, not as the columns' headers.
- Korean: the activity log's dates are a start and an end date, and a
  team's or an entry's name takes its particle as the other sentences'
  names do.

### Fixed

- **Eight sentences of the routes, the boundaries of change and the
  activity log said `--config` and `--keep-activity` as text** (0.64.0),
  read as a dash in the pages' font, where every other sentence sets a
  flag as code: they do too, and so do why the routes can't be used and a
  destination they name that no flag gives. A test fails on a sentence
  naming a flag said the other way.
- **The digest said a judged-by-nothing rule's notifications were for
  firings with no incident to judge them by**: its line of notifications
  sent says "or were judged by nothing" where an entry says
  `judged: false`, and nothing of incidents where only that kept them
  from being judged. The report's numbers keep `unjudged` as they were,
  with `by_nothing` beside it.

## [0.64.0] - 2026-11-20

Who is told what, by routes in `--config`; a team as the boundary of
change; and every source as an object, told down and back, with the
activity log filtered and exported and the daily writes through the
API. No schema change: the ledger stays at 026.

### Added

- **Routes** (docs/design/notifications.md, "Who is told what").
  `--config`'s `notify:` sends each event by its kind (`task.created`, or a
  family, `task.*`), its team (a task's or a window's, a rule's owner, an
  incident's by its labels, a goal's) and its labels (its rule's, a marked
  window's, an incident's), each at one of the values given, to
  destinations by name: the first route that takes it decides, unless it
  says `continue: true`, and what none takes goes to `default`, which
  routes need. Without routes, what happens is told as before, to the
  webhook and, with `--notify-slack`, Slack, the same rows; `digest.ready`
  and `report.ready` go where their own settings send them, routes or
  not. Every replica reads the routes, as it tells of what it writes,
  from `notify:` alone, so that an error elsewhere in the file leaves them
  in force, and puts a file in force once it has read it the same twice
  in a row, the owners worked out as it first reads them; a `notify:` that
  can't be read refuses the file, as any part of it does, and at the start
  leaves what happens told as without routes; a destination the flags
  don't give is left out, on that replica, said with its line, and the
  routes kept. Without routes in force, what is told once (a task overdue,
  a source down) is marked told as before, by the webhook and Slack
  alone.
- **Destinations by name.** A route names `webhook`, `slack`, `team:NAME`
  (`--team-digest`), a `--report-dest`'s name, or a JSON webhook of its
  own, `--webhook-dest NAME=FILE`, each post signed, when given, with its
  own key, `--webhook-dest-secret NAME=FILE` (32 bytes at least), whose
  errors name that flag. URLs stay
  in files, never in `--config`; a label picks among the routes, never a
  destination, and a notice goes to the destination its outbox row names,
  whatever it tells. `--webhook-dest` needs `--config`, and refuses a name
  `--report-dest` gives. In the chart, `serve.notify.webhookDests`, each
  from a Secret; `lapilli preflight` reads their files.
- **A destination's own words**, under `notify:`'s `templates:`: text and
  `{{.Field}}` alone, of `Event`, `Line`, `Link`, `Rule`, `Team`, `Title`,
  `ID`, `By`, `Labels` and `At`; any other field, a function or a pipeline
  is refused as the file is read. 2,000 bytes a template, 500 a value in
  it, 4,000 what it gives; a destination given twice is refused. To a
  Slack-style destination, Slack's markup as the admin wrote it, its
  `<!here>` and links working, each value's `&`, `<` and `>` escaped and
  each backquote made a quote, so that no title, rule or label mentions
  anyone, links anywhere, ends a code block or makes one with the field
  beside it; to a JSON webhook, a `text` beside the post's fields. Every
  value on one line, Unicode's format characters (zero-width ones, bidi
  controls) left out. A source's notice has its name as `{{.Title}}`.
- **The Settings page's Notifications** shows the routes in force,
  read-only, "changed there, not here", the destinations they name that
  the flags don't give, and each destination an event goes to: when a
  notice to it was last sent, how many wait, how many of the last day's
  failed or were given up on, and, to an admin, why the latest still
  unsent failed. No form changes a route.
- **`lapilli_serve_notices_unrouted_total`**, with `--config`: events that
  went where no route sent them: routes that couldn't be read since the
  start, told as without them; routes that sent an event nowhere there is,
  to the default, or else as without routes. With neither the webhook nor
  Slack, no notice is written; never to every destination. 0 with routes
  in force, each destination they name given, and a default; said on the
  Settings and Administration pages too.
- **Each destination a route may name** is, with `--config`, a place of
  the outbox's, `route:NAME`, with a loop of its own that sends what the
  outbox holds for it, whether a route names it yet or not, counted in
  `lapilli_serve_notices_total{to="route:NAME"}`, at 0 from the start.
  What waits for a name the flags no longer give is tried by the leader
  every minute, fails, and is given up on as any, named on the Settings
  page and counted on the Administration card.
- **A team as the boundary of change** (docs/design/changes.md, "A team
  as the boundary of change"; serveops.md, "A team's boundaries of
  change"). A team's editors propose changes to the rules their team
  owns, from a rule's page or for the rules picked on the rules list
  (another team's counted "not your team's"), backtest them again, and
  drop the ones they made; who owns the rule is asked of its name, of the
  rule as it is read again and of it as proposed, each with its note and
  without it, each a team they edit.
  Under a team in `--config` (`serve.config`): `approvers:`, as
  `user:NAME`, `group:NAME` or `*@DOMAIN` (not a bare name, nor `*`), who
  approve a proposal one team, theirs, owned as it was proposed and owns
  now, by its note and by its name and labels alike, and ask for its
  applied change to be undone, beside the approvers of every team,
  self-approval and `--approvals` as before; and `changes_in:`, where its
  rules are written through a ruler's or a cloud's API, within
  `--changes-in` (in the letter case its own source compares names in) or
  the file is refused, `[]` for nowhere, a rule kept elsewhere for a
  person to change. And `freezes:`,
  for everyone or a team, once or repeating as a quiet window does, in
  `--tz`: while one holds, nothing it covers is approved, approved as
  `--approvals` is lowered, asked to be undone, carried out or handed to a
  recipe; proposing and backtesting go on; one approved before waits,
  approved, said once in its history, and is carried out at the first
  round after the freeze's end. The buttons are drawn disabled with the
  freeze's name, a proposal's page says until when, the Settings page
  lists the freezes (frozen until, next, over) and the Administration
  page's Settings card says until when changes are held. Approving and
  carrying out each ask again who owns the rule: one a team owned as it
  was proposed and another, or none, owns now is approved by no one, and
  refused if approved before; carrying out asks the boundaries once more
  just before the change is sent or handed out, after its backtest runs
  again, so a freeze begun meanwhile holds it. While the file declares any
  of these, a rule's team is an admin's to move (a note's form,
  `PUT /api/v1/notes`, the rules list's "Set the team", the Cleanup page's
  move), and a rule a note moved is held by the boundaries of the team its
  name and labels give it too. A team's name is read without the spaces
  around it; two teams of one name in another letter case are refused
  where the boundaries name them, and the Settings page names a team given
  boundaries that wasn't kept. Boundaries that can't be read are said on a
  proposal's page and the Settings page, drawn all the same. Read twice,
  as the rest of the file, and kept in the ledger's `meta` table in the
  write that keeps it to the file, for every replica alike; `/config.yaml` gives them back. Agents
  propose as before: `--mcp-propose` stays an editor's of every team, or
  an admin's. No schema change: the ledger stays at 026.
- **The Sources page** (`/sources`, Administration, after Settings): every
  source serve reads, as Argo CD shows its applications, a row each: the
  rules where they are kept and what is read beside them (a cloud's
  alarms, Loki's rules), Prometheus as the runs ask it, the incidents,
  deploys and marked files, the impact signals, the on-call schedules,
  incidents and firings jobs copy in (an incidents id prefix nothing was
  written to in 14 days, or `--keep` when shorter, left out). Each says its
  kind, when it was last read well, its last error (as Run history says a
  run's: the paths of serve's files to an admin alone), the reads in a row
  that failed, the rules it gave, how far behind it is and how long its
  last read took, and a state in the incident sources' words and colours:
  working, failing, quiet, waiting, and down. Each is read, and judged, by
  its own reads: the rules failing leave what is read beside them as they
  read. A credential in an error or an address (a URL's user and password,
  a query's token, key, signature or `code`, a `password=` or `token=`
  between spaces, a JSON `"token"` or `"password"`, an `api-key:` header,
  a bearer or basic token) is `***` to everyone, an admin too. The status
  banner links to it when its failure is a source's read, and Run history
  does too. `GET /api/v1/sources?all=true` gives the same, never kept;
  without `all`, the sources firings are copied in from, as before.
- **`source.down` and `source.back`**, to the webhook and Slack, for every
  source but firings copied in (which keep `source.quiet`): down once three
  reads in a row failed, or once it has failed or been quiet past its
  bound (two runs and more for what the runs read, two days for incidents
  copied in); back once it has read well for 15 minutes. Told once while
  down, and once back, across restarts and leaders and however many look
  at once, and down again no sooner than an hour after it was told back,
  so a source that flaps is told of once. Each
  carries `source_read`: its id, kind, name, flag, state, last read well,
  the reads that failed, and its error as anyone but an admin reads it.
- **The activity log** (`/activity`, from Run history): every change kept,
  100 a page and on past them, filtered by who, kind (setting, incident,
  verdict, proposal, task, comment, note, quiet window, marked window,
  report, team, role, goal, firings, cleanup, other), team (its rules', its
  tasks', its own) and period, each change's before and after a click
  away; exported with the same filters by `GET /api/v1/activity`, as JSON
  or as CSV, whose cells a spreadsheet never takes for a formula (one that
  starts with `=`, `+`, `-`, `@`, a tab or a carriage return starts with
  `'`), worked out a few at a time, as pages are. Who reads Run history
  reads it; the roles given and taken are an admin's alone, as there.
- **`--keep-activity DUR`** (`serve.keepActivity`; unless given, 800 days,
  or `--keep` when longer, so a longer `--keep` starts as before; `0` keeps
  it all): the activity log outlives the firings it is about, never kept
  less than `--keep`: one given shorter is refused, and so is one given
  beside `--keep 0`, which prunes nothing.
- **`POST /api/v1/incidents/{id}/decide`** and **`POST /api/v1/firings/real`**:
  an impact window confirmed, dismissed or reopened, and a firing written
  down as an incident it caught, through the API as their forms do them,
  an editor's, with the forms' checks (a firing older than 14 days an
  admin's). Proposing stays a rule's page's and MCP's (behind
  `--mcp-propose`), and approving the proposal's page's.

### Changed

- `--config`'s top-level keys are teams, windows, reports, grants, goals,
  marked, **freezes** and **notify**, and its refusals say so.
- The sidebar's Administration has Sources after Settings.

## [0.63.2] - 2026-11-20

A security release, after a review of 0.63.0 from an attacker's point of
view and the rest of the week's reviews, each fix of code with a test that
failed before it; with it, "Judged by nothing" for a rule entry's
`judged: false`, and Spanish's tables at a laptop's width. No schema
change: the ledger stays at 026.

### Fixed (security)

- **A pull request's PromQL could read past the backtest port's bounds,
  written as the history reads it otherwise than the port's checks did**
  (affected: 0.61.0 to 0.63.1). Prometheus ends a `#` comment at a `\r` as
  at a `\n`, and reads one in a selector's braces and a range's brackets
  as anywhere outside a string; the port's checks ended one at `\n` alone,
  and read none in braces or brackets, so that what followed a comment's
  `\r`, or a `}` or a `]` and a quote in one, was a comment or a string to
  them, and code to Prometheus. With VictoriaMetrics, a `--prometheus` the
  docs name, MetricsQL reads a `\` in a name as escaping the character
  after it, a backtick among them (``up\` or {__name__=~".+"} or up\` ``
  was a string to the checks), `or` in a selector's braces as joining its
  matchers (`{__name__='up' or __name__=~'.+'}`, one string to the
  checks), an offset after any expression (`rate(up[5m]) offset 1400d`),
  `WITH` templates, `1doffset` as `1d offset` and `"__\name__"` as
  `__name__`; and Prometheus with unquoted UTF-8 names reads a name that
  isn't ASCII (`温度 offset 1400d`). Either way a rule backtested through
  the port could read every series, 1,400 days back, or at a time of its
  own (`@ 0`), with serve's access, and be taken. The checks now read
  comments as Prometheus does, and the port takes only what every history
  reads alike: no `\r`; outside its strings, ASCII names, numbers and
  durations as Prometheus writes them, operators and brackets alone; an
  `offset` or `@` right after a selector, a range or a subquery; and none
  of MetricsQL's own (`WITH`, `or`, `and` or `unless` in a selector's
  braces, `default`, `if` and `ifnot` between two expressions, which made
  `x ifnot {job=~".+"}` read every series with a job, `keep_metric_names`,
  `__graphite__`, a `\` in a label's name in quotes). A string with an
  unescaped quote of its kind inside isn't one string to the checks any
  more. A rule's own comments are taken as before, and rules as alerting
  rules are written read alike, as checked with Prometheus's parser and
  MetricsQL's.
- **Anyone who could reach the backtest port could keep a job from reading
  or cancelling its backtest** (affected: 0.61.0 to 0.63.1). Each `GET`
  and `DELETE` was charged to the client's address before its key was
  checked, 20 at once and one a second after. Behind an ingress, or a
  LoadBalancer, every request comes from one address: 20 requests with no
  token, for an id made up, made every job's poll and cancel a 429 until
  `SERVE_WAIT` ended it, its backtest running on. Each replica now holds
  the backtests' key hashes, those it queued and the ledger's, listed a
  second apart at least: a job's id and key, known, are read whatever else
  comes from its address. A pair not known waits up to 2 seconds for a
  listing begun after it came, so that a backtest queued through another
  replica is found, on its first poll, or, when guesses from the address
  everyone shares have taken its 20 waiting, on the next, after a 503 with
  `Retry-After` and a listing begun, never said not found; only those
  waits are charged to the address. Guesses read no backtest's row.
- **A token that had started a backtest could be sent again and again,
  past its entry's hour** (affected: 0.61.0 to 0.63.1). A request refused
  before its body was read gave its entry's hour back, one whose token
  had started a backtest already among them: whoever held such a token, a
  job's for up to an hour, could send it as often as the port took
  requests, each a 409 that cost a signature checked and four reads of the
  ledger, the entry's `hourly` untouched. A token that started one keeps
  the hour now; a full queue and full reads still give it back.
- **One who could send from many addresses began everyone's limits at the
  backtest port afresh** (affected: 0.61.0 to 0.63.1). The limits by
  address (of a backtest not known yet, waiting for a listing) and by
  backtest (its reads) were kept for 4,096 at most, and past them all let
  go at once, the spent ones among them. Past them, only those refilled
  to their burst go now, a second apart at most; while none can, an
  address or a backtest not kept is refused, as one past its bucket is.
- **A CI's token that names no key was tried against every key of its
  issuer's** (affected: 0.61.0 to 0.63.1). A token sent to the backtest
  port with no `kid`, of an issuer an entry names, was checked, before
  anyone was known, against each of the issuer's keys, a hundred at most,
  RSA of up to 4,096 bits. The CIs' tokens name their key: one that
  doesn't is tried against the first three of its issuer's keys now.
- **One repository's pull request could take the place of another's
  backtest** (affected: 0.61.0 to 0.63.1). A newer commit's backtest
  supersedes the one waiting for the same entry, repository and pull
  request, the repository by the name its token gives, kept as "" past
  200 bytes or with what doesn't show: two repositories an entry pins,
  each named so, were one, and one's pull request 5 superseded the
  other's. A token whose repository's name isn't kept supersedes none
  now, as GitLab's doesn't.
- **A pull request's rule could read `target_info` through `info()`**
  (affected: 0.61.0 to 0.63.1). Prometheus's `info(rate(x[5m]))` reads
  `target_info` beside the metric it names, to join its labels on, a
  metric the rule never named: past the one metric a selector names that
  the port holds a rule to. `info(` is refused at the port now, written in
  any case, a comment or spaces before its `(`.
- **A Grafana rule's query could read a day past the backtest port's
  reach** (affected: 0.63.0 and 0.63.1). When a data source query of a
  Grafana rule returns nothing, the replay asks it whether it returned
  anything in the day before (a day's subquery of it, or, in LogQL, the
  query itself over the day), which the port didn't count: a query that
  read 8 days back read 9; and, in PromQL, a subquery at the group's
  interval, 86,400 points a series at `interval: 1s`, past the 11,000 a
  subquery is held to. That day is added to what each data source query
  reads back now, 8 days in all, and its points counted: a Grafana rule's
  group is evaluated every 8 seconds or more. A classic condition's
  window was counted already, in the queries its reducers ask.
- **The lane's requests to a Loki weren't given up on** (affected: 0.63.0
  and 0.63.1). Each range query the lane asks a Prometheus carries
  `timeout=1m`, so that a shared Prometheus gives up on a pull request's
  query before its own default; a Loki takes no such parameter, and a
  pull request's LogQL that Loki took long over held one of the lane's
  turns, and the share kept for it, until `--backtest-timeout`. Each of
  the lane's requests to a Loki is given up on after a minute now, from
  when it is sent to its answer read.
- **A rule file whose name begins with a dot, or past 16 MiB, went past
  every gate of the backtest recipes unbacktested** (affected: 0.47.0 to
  0.63.1). Prometheus's `rule_files` glob loads `alerts/.page.yaml` and
  `alerts/..page.yaml` for `alerts/*.yaml`, and `alerts/.d/page.yaml` for
  `alerts/*/*.yaml`; its rulefmt reads a file in UTF-16 with its BOM, and
  one of any size. But `--rules` passed over every name beginning with a
  dot, and a rule file past 16 MiB below the top, and the recipes' serve
  mode sent no hidden file: a pull request adding a rule so passed
  `EXPECT_REQUIRED`, `FAIL_ON_EXPECTATIONS` and `FAIL_ON_ERROR`, and no
  comment was posted. Such a file is read and backtested now, a long one
  refused, and a hidden one in UTF-16 said in serve mode as a rule file
  that isn't UTF-8 text is (Changed).
- **A file of a pull request's rules could go unbacktested on a macOS or
  Windows runner** (affected: 0.32.0 to 0.63.1). Two paths under `PATHS`
  (and, from 0.63.0, `LOKI_PATHS`) that differ only in case are one file
  on a file system that folds case: `git archive | tar -x` wrote one over
  the other, the first's rules were never backtested, and
  `EXPECT_REQUIRED`, `FAIL_ON_EXPECTATIONS` and `FAIL_ON_ERROR` passed.
  Where the runner's file system folds case, such a commit isn't read
  now, the pair named in the job's log and not the comment; nor, on any
  runner, is one of more files than its file system holds of it once
  written (a letter past ASCII in another case). On Linux both are
  written, and read, as Prometheus reads them.
- **A pull request could pick the incidents it was judged by** (affected:
  0.32.0 to 0.63.1). `INCIDENTS` was read from the commit the pull request
  and `TARGET` share last, which its author picks by where they branch
  from: where the job checks out the pull request's own commit, not a
  merge (`TARGET_BRANCH`, GitLab without merged results), a pull request
  branched from before an incident was written down was judged without
  it. It is read from `TARGET` itself now, the commit the pull request
  goes into as it is, and `gitlab.sh` without merged results takes the
  target branch (`CI_MERGE_REQUEST_TARGET_BRANCH_NAME`, fetched with the
  job's credentials) for `TARGET` in place of the merge base. The rules
  are compared against those where it branched, as before, and so are
  the expectations of the rules in place: a pull request is held to the
  expectations in its own commit, the target's where the job checks out
  a merge (a merged results pipeline, or a branch rebased).
- **Answering only "needed action" took away another person's
  "unrelated"** (affected: 0.63.0 and 0.63.1). A client writing a verdict
  whole, `"unrelated": false` beside its answer, of a firing a run had
  merged another into, took away the verdict said of the other, which the
  merged firing held, and the firing turned white. "Unrelated" is said now
  only when it differs from what the firing's own verdict holds. A verdict
  held, undone at its own alert and start as the page of verdicts to look
  at again undoes it, judges the firing holding it again at once.
- **Writes of one person could push the admins' changes of the settings
  off the Settings page** (affected: 0.6.0 to 0.63.1). Its "Changes" were
  those of a setting among the latest 100 changes of every kind: a
  hundred writes of one editor, notes, comments or verdicts, left none to
  show. The settings' changes are asked for apart now, the latest 20.
- **`lapilli preflight` printed an OCID it hadn't learned whole**
  (affected: 0.60.0 to 0.63.1). Another compartment's, a dynamic group's
  or a policy's OCID in what OCI said was shown, though an ARN, a GUID and
  a long number weren't. Each OCID, a word's character before it or not
  (`%3Docid1…` in a URL), is said by what it is when the run learned it,
  and as `[an OCID]` otherwise.
- **`lapilli preflight` asked Azure of a resource id with `..` in its
  path** (affected: 0.60.0 to 0.63.1). An alert rule's document naming,
  in any of its strings, an id such as
  `/subscriptions/…/resourceGroups/other-rg/providers/x/../../..` had the
  write identity's permissions asked at what the path led to, another
  subscription's among them (a read; nothing is written). An id with a `.`
  or `..` for a part of its path is unknown now, and isn't asked.

### Fixed

- **Judging read every verdict's notes for nothing** (affected: 0.63.0
  and 0.63.1). Each replay judged by people's verdicts, every backtest,
  comparison and try among them, read both notes of each verdict of its
  period, up to 500 characters each, and the firing it was kept by, none
  of which judging takes. It reads a verdict's parts alone now.
- **A ConfigMap's item with a nested path wasn't read** (affected: 0.5.0
  to 0.63.1). Mounted, an item whose `path` is `team/x.yaml` is a link
  beside `..data` to a directory of the volume's own (`team ->
  ..data/team`), and `--rules` followed no link to a directory: its rules
  were left out, and nothing said so. Such a link is followed now, beside
  a `..data` link alone, to a directory right in the volume's data, each
  once, however many links lead there.
- **`lapilli preflight`'s questions of what an identity may write had no
  time limit of their own** (affected: 0.60.0 to 0.63.1). Each was asked
  within its caller's 10 seconds; the client gives up after as long
  itself now.
- **A `Retry-After` of 0 could loop the recipes' requests to serve**
  (affected: 0.61.0 to 0.63.1). With `SERVE_RETRY_MIN=0` and
  `SERVE_POLL=0`, as tests set them, a 429 or 503 with `Retry-After: 0`
  was asked again at once, as often as `SERVE_WAIT` let. It is waited a
  second at least now, whatever both say.
- **`gitea.sh`, the recipes' README and docs/design/backtests.md said
  Forgejo's runner reads no `::add-mask::`** (0.61.0 to 0.63.1). It does,
  as act does, and they say so now; the recipe still doesn't send it, as
  nothing it runs prints the token.
- **A day's strip drew the firings of a rule judged by nothing as caught
  nothing** (0.63.0), with no named signal: it read the day's starts
  apart by rule only for named signals.

### Changed

- **A hidden rule file is read, and a long one refused.** `--rules DIR`,
  the versions of `--rules-history` and the backtest port read a file with
  a name in its path beginning with a dot, `..` too, when it holds rules,
  as Prometheus's glob loads it, and go into a directory so named; one
  that holds none, or doesn't read, is passed over wherever it is, at the
  top too, and isn't counted in what is said passed over, so a
  `.gitignore`, a `.github/` or a version's `.commit` beside the rules
  breaks and says nothing; one Lapilli's reader refuses is said. Never
  read are a `.git` directory, and one whose name begins with `..` beside
  a `..data` link, as a Kubernetes volume keeps a ConfigMap's files. A
  file past 16 MiB named as rule files are (`.yml`, `.yaml`, `.json`,
  `.rules`) is refused wherever it is, where one below the top, or
  hidden, was passed over: one in a `.venv` beside the rules too, when
  `--rules` names the directory both are in; name the rules' own
  directory to read them alone. The backtest recipes send hidden files to
  serve, `..` names among them, but one that isn't UTF-8 text and has no
  BOM (a `.DS_Store`); one in UTF-16 or UTF-32 is said, as a rule file
  that isn't UTF-8 text is.
- **A rule entry's `judged: false` is "Judged by nothing" in English**, on
  the rules list's badge, a rule's page and the Goals page, as the
  settings' table of entries says it: "Not judged" was a capital and a
  sign from "? Not judged", the firings with no record. Its firings say
  so too, not "no record to judge it by", and the legends with them.
- The Goals page's line of what people moved lists its parts as Japanese
  lists things, with 、.
- Spanish's rules list fits a laptop's 1,280 px, and the overview's
  heatmap 1,200 px, as the other languages' do: its headers, single long
  words, are a little smaller. Under 1,280 px the rules list still
  scrolls in its box.

## [0.63.1] - 2026-11-20

The forms for people's verdicts on firings (0.63.0) on a few lines a row,
and their words in Korean. No schema change: the ledger stays at 026.

### Changed

- **A row of the firings list is 96 px again**, at 1,280 px, from 179
  (331 with a note): what people said of a firing is its own column, "What
  people said", "needed action" a select that says its answer and a
  button on one line; the stone's words break between words, Korean's too,
  not a syllable a line, and "paged no one" may take two lines. A rule's
  firings, 55 px from 99.
- **The bulk "needed action" starts on "pick one"**, which the browser
  holds back; it started on yes, so that one press marked every firing
  picked as needing action. Its "none" is "clear the answer". A bulk
  answer sent with no choice writes nothing. "Answer" is "Save".
- **Korean**: 알림 묶음 for an episode, not 에피소드; 판정 제외 for a rule
  entry's `judged: false`, not 판정 안 함, a letter from 판정 안 됨 (no
  record); “실제 문제였음” quoted as a button's name; 무관: INC-111, not
  INC-111와 무관함; a rule's precision as Lapilli judged it says only what
  people moved, as the Goals page's line of what people moved does now,
  where it said every zero. Japanese and Spanish said the two not-judged
  states alike too (判定対象外, "Fuera del juicio" now); Spanish's
  Notified is "Avisos".
- The Episodes column fits where the tables did before it: the rules list
  at 1,070 px, the overview's heatmap at 1,200 px (a rule's name cut
  sooner under 1,280 px).
- The accessibility check reads people's verdicts and the episodes: its
  server has them.

## [0.63.0] - 2026-11-20

Judging, steps 1 to 4 (docs/design/judging.md): a lead and `require:` of
a signal's or a rule entry's own, a signal taken away judging no one,
episodes beside firings, and a person's verdict on one firing, kept apart
from Lapilli's own stones. Backtests, steps 2 and 3
(docs/design/backtests.md): Bitbucket Pipelines' ID token, and
Grafana-managed and Loki rules on the data sources the rules in effect
read already, at the backtest port. And a pull request's rule there read
back no further than its group's `query_offset` and 8 days allow.

Before upgrading:
- **Ledger schemas 024, 025 and 026**, after 023, in one start: keep a
  copy of the ledger's disk, or of its database, first
  ([Going back after a schema upgrade](charts/lapilli/README.md#going-back-after-a-schema-upgrade)).
  A lapilli before this one doesn't open the upgraded ledger.
- After the upgrade, the leader counts the episodes again a week at a
  time, the newest first; until then those weeks say their episodes
  aren't counted yet. No stone changes.

### Added

- **Grafana-managed and Loki rules at the backtest port**
  (docs/design/backtests.md, step 3; open question 2 decided), on the data
  sources the rules serve runs read already, so that a pull request makes
  serve read nothing with its credentials that it doesn't read for its own
  rules. A Grafana rule is taken only when each of its queries names, by
  its uid exactly (its case, its spaces, its length), a data source whose
  queries the replay of a rule in effect asks, and asks it as that rule
  does: PromQL of `--prometheus`, or LogQL through `--grafana`'s data
  source proxy (`--loki` without it). A uid serve's Grafana token can read
  but no rule in effect asks, one only a query beside a condition or a
  paused rule names, Grafana's own (`grafana`, `-- Mixed --`,
  `-- Dashboard --`) and a uid Grafana makes none of are refused; an
  expression's model (`__expr__`) reads nothing. A Loki ruler's rule comes
  from a file the job marks `"loki": true`, read as `--loki-rules` reads
  one, and is taken only where serve has `--loki` and its rules in effect
  hold one, asked with serve's tenant. A rule refused for what it reads is
  said to read "a data source no rule in effect reads", whatever why,
  naming no uid or data source of serve's; a Grafana rule's query held to
  a bound below is said by its refId, nothing of it quoted, as what the
  replay asks has serve's data source's scrape interval put in for
  `$__rate_interval`. Beside every bound the Prometheus path has: a
  Grafana rule has 8 queries of data sources and 16 queries and
  expressions at most, each query's `relativeTimeRange` 8 days back at
  most, and each query asked held as a rule's, the end of its
  relativeTimeRange added to what it reads back; a LogQL query is a metric
  query, each stream selector naming its streams by a label matched with
  `=` to a value, its range and offset 8 days back at most, with no
  subquery and no `@`, its comments in `#` alone and no `'` outside a
  string (Loki also reads `//` and `/* */` as comments, and a quote in one
  would hide the rest from these checks). The lane asks a Loki through a
  client of its own at its share of that Loki's rate, kept for it while a
  backtest asking that Loki runs, and all its requests within a quarter of
  `--parallel` at once. The recipes send `LOKI_PATHS` (the action's
  `loki-paths`) so, with `PATHS`, in serve mode. `lapilli preflight` times
  the runs at `--max-rate` less the lane's share for every API they ask, a
  Loki and Grafana's data source proxy too, and its estimate's backtest
  changes Grafana and Loki rules as well. MCP's `compare` is as it was: no
  Grafana rule, and LogQL only as a change to a Loki ruler's rule in effect.

- **Bitbucket Pipelines' ID token at the backtest port**
  (docs/design/backtests.md, step 2; open questions 3 and 4 decided). A
  trust entry with `ci: bitbucket`, or known from a workspace's issuer,
  `https://api.bitbucket.org/2.0/workspaces/WORKSPACE/pipelines-config/identity/oidc`,
  must pin `repositoryUuid` (a list allowed) and `workspaceUuid`, each
  lowercase in braces as Bitbucket writes them, or serve doesn't start.
  Bitbucket writes the workspace's own audience
  (`ari:cloud:bitbucket::workspace/UUID`) in every token beside the step's:
  a Bitbucket entry takes its audience alone or beside the pinned
  workspace's, never a third, and every other entry still one alone;
  `ari:cloud:bitbucket:...` is refused as an entry's audience. Its token
  has no `jti` and names no pull request: known by what it says, it
  supersedes no backtest. The Run history shows the repository by its
  UUID. `bitbucket.sh` sends the step's token given as `LAPILLI_ID_TOKEN`
  (`LAPILLI_ID_TOKEN=$BITBUCKET_STEP_OIDC_TOKEN`, from a step whose
  `oidc: audiences:` names serve's alone), refuses at once to send one
  whose `aud` (its payload read, not verified) names Bitbucket's
  workspace audience alone, which serve would refuse and whatever trusts
  the workspace takes, and keeps `BITBUCKET_STEP_OIDC_TOKEN` out of what
  it runs in serve mode; `lapilli preflight` says such an entry's CI as
  `bitbucket`. A token serve takes is one whatever trusts the workspace's
  own audience takes too: the docs say to have those services (an AWS
  role made by Bitbucket's guide) trust an audience of their own instead.
  Azure Pipelines stays on the plain token (open question 3 decided): new
  service connections' tokens are issued by the Microsoft Entra tenant,
  an issuer every app of it shares, for an audience the job can't choose,
  and the Azure DevOps issuer retires on 2027-07-01.

- **A lead of a signal's or a rule entry's own, and `require:` on an
  entry** (docs/design/judging.md, step 1; signals.md's open question 4
  closed). A named signal may give a `lead`, how long before one of its
  windows' start a firing still catches it, for every rule judged against
  it, and a rule entry one for its rules against its signals' windows, in
  place of each signal's: from 0 to 24h, each following the one above it
  (an entry its signal's, a signal `--lead`). A cause that fires before
  its incident, memory pressure an hour before the errors, can be white
  without raising `--lead` for every rule. Incidents written down, copied
  in or read from a file, the windows of the signal with no name and those
  of a signal no longer read keep `--lead`. Set by `--impact-lead
  NAME=DUR` in `lapilli verdict` and `serve`, `lead:` on a signal and on a
  rule entry in `--impact-file`, the Lead field of a signal's form on the
  Settings page and `lead:` in its rule entries' YAML, all given back by
  `/settings.yaml`; nothing in `--config`. `require: [LABEL, ...]` on an
  entry, opt-in, are labels a firing of its rules and a window of its
  signals must both carry for the one to catch the other, so that a node's
  alert with no `service` catches no service's window. `lapilli verdict
  --impact` judges by them as serve does; its text says each signal's lead
  beside its `for` when one is given. The report's
  `settings.impact_signals[].lead_ns` and `settings.impact_rules[].lead_ns`
  and `require` (each left out when it follows, or for none),
  `/api/v1/summary`'s `settings.impact_signals[].lead` and
  `settings.impact_rules[].lead` and `require`; numbers.md's "incidents
  caught: counted again" reads them. Everything that reaches back from an
  incident to the firings it can catch reaches by the longest lead in
  effect (the incidents a span reads, the spans a run, a read again, a copy
  and a tie of the unnamed signal's windows judge, the near misses' replay,
  an incident's timeline). A lead changed alone, on the command line or the
  Settings page, judges again only the spans of the windows it touches,
  from the longer of the old and the new lead before each to its end,
  reading nothing from Prometheus, in the write that keeps it; `require:`
  changed judges every firing again, as an entry's match does; either
  holds waiting proposals until they are backtested again. **Changed with
  the defaults:** what is worked out once a week is whole, `goal.missed`,
  the goals' tasks and their `/metrics`, and the digest, wait for the runs
  to pass the week's end (or an occurrence's) by the most, over each
  signal and the rules judged against it, of its lead and its `for`
  (`--lead` and `--impact-for` of the signal with no name, `--lead` with
  written incidents alone), and `--settle` after that, so that no stone
  of the week turns after it was told: with the defaults and an impact
  signal, from about 00:00 on Monday to about 01:20; the digest, at Monday
  09:00, isn't later. A ledger with no runs, whose firings are copied in,
  waits for its sources to have written through the week's end and
  `--lead` past it. With no lead set anywhere and no `require:`, every
  stone and number is as it was. **Ledger schema 024**, after 023:
  `impact_signals.lead_ns`, `impact_rules.lead_ns` and
  `impact_rules.require_labels`, NULL for every signal and entry kept
  before. A lapilli before this one doesn't open a ledger it has migrated:
  keep a copy of its disk, or of its database, first ([Going back after a
  schema upgrade](charts/lapilli/README.md#going-back-after-a-schema-upgrade)).

- **Episodes beside firings** (docs/design/judging.md, step 3). A rule's
  firings are counted as episodes too, one problem however many alerts it
  is: its firings with the same values of the match labels in effect,
  each starting while another is on or within `--refire` of the latest end
  among them so far, parted at midnight in `--tz`, so that an alert on
  twelve pods, or one that flaps, is one episode a day, not twelve or
  twenty stones. An episode is white when any of its firings is white, and
  judged by its first firing's record. They change no number of today's:
  precision, noisy, the badge, the suggestions and what a catch counts for
  stay by firings, and noisy by episodes, by the same thresholds, is shown
  and decides nothing. The report (`lapilli verdict --json`,
  `/api/v1/report`) gives `rules[].episodes` (`total`, `white`,
  `unjudged`, `noisy`), each firing's `episode` (numbered in the report)
  and `day`, and `settings.zone`, so that numbers.md's new "Episodes"
  counts them again; the CSV an `episode` column, at the end; `/metrics`
  and `--metrics` `lapilli_rule_episodes` and
  `lapilli_rule_episodes_caught`; `/api/v1/summary` each row's `episodes`
  (`total`, `caught`, `unjudged`, `precision`, `noisy_rules`) and each
  marked kind's `episodes`, `episodes_caught` and `episodes_precision`;
  MCP each rule's `episodes` and `noisy_episodes`, and each firing's
  episode. The rules list has an Episodes column and `noisy:episodes`; a
  rule's page its episodes, their precision, and "Noisy by firings, not by
  episodes" when the two differ, and of its latest firings the episode
  each is of; the overview's noisiest rules their episodes; the digest a
  line of the noisy rules' episodes. Goals take `episodes.total` and
  `episodes.precision`, each with `each: KIND` (an episode during a kind
  when its first firing is), the latter with `no_worse_than: before`. **Ledger
  schema 025**, after 024: `firings.episode` and `episode_white`, and
  `episodes` and `episodes_white` on `days`, `weeks`, `marked_days` and
  `marked_weeks`. Each write walks the firings of the days it counts once
  more; the upgrade, and a ledger opened in another `--tz`, count the
  episodes again a week at a time, the newest first, in the leader's
  round, and until then those weeks say their episodes aren't counted yet
  (`episodes: null`). No stone changes. A lapilli before this one doesn't
  open a ledger it has migrated: keep a copy of its disk, or of its
  database, first.

- **A person's verdict on one firing** (docs/design/judging.md, step 4,
  item 3). On Alert history, a rule's latest firings, and from each rule
  an incident's timeline shows, a person may say a white firing is
  **unrelated** to what it caught, a disk alert during another service's
  outage, which turns that firing alone black: the incident is caught by
  the other firings alone, or missed. And whether a firing **needed
  action**, yes or no, which changes no stone and is counted. "Unrelated"
  is an editor's of every team and an admin's; "needed action" a team's
  editor's too, of their team's rules, whose write leaves "unrelated" as
  stored (changing it is 403). Each part keeps who last wrote its answer
  or its note, when, and a note (500 characters, one line); another answer
  with no note given drops the note of the one before. One who isn't an
  admin speaks of the last 14 days' firings alone; each person makes 30
  verdict writes an hour, forms, a bulk answer of 100 firings and the API
  alike, past which 429 with `Retry-After` and nothing written; the activity
  log shows each person's verdicts of an hour as one line. A verdict is kept
  by its firing's alert and start, so a run that starts the firing a step
  later, or a window read again or confirmed, takes it along, marked to look
  at again when what it was said of moved: the overview counts those, the
  Cleanup page lists them. Every replay carries the verdicts of its
  period on both sides, a proposal's backtest and `backtest_again`,
  `compare`, a pull request's backtest, the Settings page's tries and the
  digest's suggestions and near misses, so a longer `for` can't look
  better on the firings people called coincidences; a verdict said, or
  undone, in a proposal's period since its backtest makes it stale. `GET`,
  `PUT` and `DELETE /api/v1/verdicts` and `PUT /api/v1/verdicts/bulk`,
  `GET` 31 days and 20,000 verdicts at most, one such request at a time,
  as a report with its firings is; MCP's `firings` and `rule` read them, and
  no tool sets one. The report gives `rules[].unrelated`, `action_yes`,
  `action_no`, `firings[].unrelated`, `action` and `incidents[].real`; the
  CSV `unrelated` and `action` at its end; `/api/v1/summary` each row's
  `people` (`unrelated`, `unrelated_excused`, `real`, `real_white`,
  `real_excused`, `caught_by_lapilli`, `precision_by_lapilli`, and `action`
  with its `share` once 80% of the row's firings are answered); `/metrics`
  `lapilli_rule_action`; the digest a line. Goals take `action.share`, a
  week under 80% answered missed, not unknown, and `stones: lapilli` on a
  precision goal to hold it to Lapilli's stones alone; the Goals page, a
  goal's `goal.missed` notice and its task's "no longer missed" say what
  people's word moved.

- **Rules judged by nothing** (docs/design/judging.md, step 4). A rule
  entry may say `judged: false` in place of signals, in `--impact-file` or
  on the Settings page: a watchdog, a forecast. Its rules' firings are
  counted, never white nor black, in no precision, never noisy, and catch
  nothing, written incidents included; the report says them `not_judged`
  (`rules[]`, `firings[]`), the summary `not_judged` beside `unjudged`,
  the rules list and the rule's page "Not judged". A file whose entries all
  say it needs no signal, and goes with `--impact PROMQL` or none.
  **Ledger schema 026**, after 025: `verdicts`; `firings.person` and
  `unrelated`; `unrelated`, `unrelated_excused`, `real_white`,
  `real_excused`, `action_yes`, `action_no`, `action_yes_black` and
  `action_no_white` on `days` and `weeks`; `incidents.real_of`;
  `impact_rules.judged`, reserved and unused; `goals.stones`. No stone
  changes until a person says something. A lapilli before this one doesn't
  open a ledger it has migrated: keep a copy of its disk, or of its
  database, first.

### Changed

- **"This was real" is held to its firing, and to 14 days** (docs/design/
  judging.md, item 3). Its forms, one firing's and the firings picked,
  carry the firing's series and start beside its id, and are refused, 409
  ("this firing changed since the page was read: look again"), when the
  id is another alert's firing by then: a run gives its firings again,
  and SQLite a gone row's id to another, so a page read before a run could
  write down another alert's firing. A post without them is refused so
  too. One who isn't an admin presses it on the last 14 days' firings
  alone (403 beyond, an admin's); writing an incident down by hand stays
  an editor's at any age. It refuses a firing a person said is unrelated
  (409): undo that first. The incident it writes notes the firing it was
  written from.

- **A signal taken away judges no one** (docs/design/judging.md, step 2,
  item 5; docs/upgrading.md, "Windows of a signal no longer in effect").
  A named impact signal no longer in effect, taken away on the Settings
  page, dropped by a start without it, or set aside at a start past
  eight, keeps its windows, with its name. They kept every rule's record,
  as incidents written down, and any rule caught them: in their weeks and
  the two after each, a rule an entry ties only to a signal with `record:
  false` was judged, its firings black, where `lapilli verdict` with the
  same rules and file, which never read that signal, leaves it unjudged.
  Now such a window keeps no rule's record, with named signals in effect
  or none, and only the rules no rule entry names catch it, on the labels
  it was found with, from `--lead` before it (the rule entries are read
  for it with no named signal in effect too); a rule whose entry names no
  signal in effect is judged by incidents written down alone. An editor
  or an admin who confirms one that was a real outage writes it down, as
  ever: it keeps everyone's record, and is caught as its window was.
  **Changed on upgrading:** the weeks a window of a signal no longer in
  effect was on in, and the two after each, may show firings counted and
  not judged where they were black, so a precision, a noisy badge, a
  suggestion or a goal that needed that record is not known there; and a
  rule an entry names no longer catches such a window, its firing black,
  or not judged, where it was white. The leader's first round after the
  upgrade judges their spans again, once, whatever replica leads, and
  logs it (`windows of impact signals not in effect judged again, once`,
  with `windows`). Serve agrees with a fresh `lapilli verdict` on every
  rule's record; their stones differ only where a rule with no entry
  catches such a window, which serve keeps and `lapilli verdict` can't
  read. A window's page and the page of a confirmation of one say it, a
  rule's page says of an entry naming no signal in effect that its rules
  are judged by incidents written down alone, the Incident sources panel
  has a row of such windows in four weeks and of which signals, and MCP
  gives `incidents[].signal_not_in_effect` and `rule`'s
  `impact_signals.entry_signals_not_read`; numbers.md's "A white or a
  black stone", "incidents caught: counted again" and "No record" count
  them. A ledger that never kept a window of a signal no longer in effect
  judges as before, and the windows of the signal with no name are as
  they were. No schema change: the ledger stays at 024.

### Fixed

- **A pull request's rule at the backtest port read back as far as its
  group's `query_offset` said** (docs/design/backtests.md, "As built (step
  3)"). From 0.61.0, a group's `query_offset` moved each of the rule's
  queries back by that much, past the 8 days the port holds what a rule
  reads back to: it is added to them now, and 8 days at most.

## [0.62.1] - 2026-11-19

The pages in words a person reads: Korean without the ledger's name and
with one name for each thing, a flag's name in a sentence set as code,
three forms' syntax behind "How to write it", and what the review of
0.62.0 left. No schema change: the ledger stays at 023.

### Changed

- **Words a person reads, not Lapilli's own** (docs/design/ui.md, bundle
  64):
  - Korean doesn't name the ledger (원장): "아직 기록된 발화가 없어요", a
    limit as the reader's ("메인터넌스 윈도우는 %d개까지 둘 수 있어요"), 기준
    시간대 for its time zone. A marked window is 이벤트 구간 everywhere, a
    rule retired 폐기 (not 은퇴 or 없애기), a report's section of the hours asleep 자는 시간 오탐
    as the overview's tile, not 자는 중 깨움; Outcomes' lede says what it
    checks, not that Lapilli asks itself.
  - A flag's name in a page's sentences is set as code, in every language:
    in the pages' font two hyphens read as one dash. (A form's refusal,
    when one names a flag, says it plain still.)
  - A repeating window's, a report schedule's and a marked window's kind's
    syntax is behind "How to write it" under its form.
  - A proposal with no record says "or found from user impact", and search
    finds an impact window by its title as kept ("--impact") too.
  - A table whose labels column can make it scroll sideways (the firings, a
    rule's latest, quiet and marked windows, teams) takes the focus, so it
    can be scrolled by the keyboard in Safari too.
  - A one-line field grows with its line, as with a large minimum font
    size, past 36 px.

## [0.62.0] - 2026-11-19

`lapilli preflight` reads the backtest lane: its trust file as serve
reads it, its issuers' keys, and its share of the rate. And the pages as
0.61 read on a real deployment: one line for whoever may not change
things, the rules list on a laptop, a rule's team said, the pages saying
what to check, and Safari's fields and the firings list fixed. No schema
change: the ledger stays at 023.

### Added

- **`lapilli preflight` reads the backtest lane** (docs/design/preflight.md,
  step 3). With `--backtest-listen`: `backtest.trust` says what serve does
  with `--backtest-trust` at start, read by serve's own reader: it takes
  the file, naming each entry, its CI and the claims it pins, never what a
  claim is pinned to, a plain token's file read as serve reads it and said
  long enough, never its text or length; or it stops, a problem saying
  where (the line, the entry, the key) and why in Lapilli's words, never a
  value the file holds: `serve.ReadTrust` refuses a file as a
  `TrustRefused`, its message to serve's log as it was. The file needs no
  credential, and is read with `--offline` too. `backtest.issuers` asks each
  issuer the file names for its discovery document and keys, once, four
  at a time, as a token's first check does (its issuer to the byte, its
  keys at its own origin or `jwks_url`, no redirect, 1 MB and 100 keys),
  GETs of exactly those through the allowlist, counted as `backtest` in
  `asked.by`: ok when a key verifies a token of an algorithm it names. An
  issuer of no public CI's, and a `jwks_url`, are said as
  `[--backtest-trust's issuer N]` and hidden wherever the report would say
  them. `estimate.backtests` says how many requests a backtest fits in
  `--backtest-timeout` at the lane's share of `--max-rate`, and how many
  days of ten of the rules changed fit, their expressions or their `for`
  alone, counted by the count the port refuses one by
  (`serve.BacktestRequests`); a problem only when one rule added, taken
  away or its `for` changed over a day doesn't fit; the hourly runs
  are timed at `--max-rate` less `--backtest-share` too, as while a
  backtest runs the whole hour (`while_backtest`), a run past `--every`
  only then unknown; and memory says the lane's reads beside the runs'
  (`backtest_bytes`). A trust file serve refuses is the item's problem,
  exit 1, where serve exits 2. Without `--backtest-listen`, the report is
  what it was, to the byte.

### Changed

- CI on a pull request: a new push cancels the run in progress, the tests
  on Postgres run in two shards each (`test/shard.sh`), each in half the
  time, and an end-to-end job runs only when the changes reach it
  (`test/e2e/reached.sh`), none for the docs alone. On main every job
  runs, as before (CONTRIBUTING.md).
- **The pages for whoever may not change things, the rules list on a
  laptop, and a rule's team said** (docs/design/ui.md, bundle 62), from
  0.61 read on a real deployment:
  - Whoever may change no rule sees a line saying why in place of the
    rules list's bar, and no column of boxes to pick a rule by; whoever
    may make none of What to look at's tasks, for one reason, a line under
    it, not a disabled button on each. A team's editor, who may do some,
    still sees each disabled, with why.
  - Narrower than 1200 px the rules list leaves out its 12 weeks' column,
    which each rule's page has, and its label chips are a little smaller,
    so that a row isn't three chips of two lines each tall.
  - The rules list says, under a rule's team, how it was found: by its
    note, its name or its labels, as the rule's page does.
  - A proposal's backtest says how many more or fewer firings as a number
    alone, as a rule's page does, not better or worse: a change may mean to
    fire more.
  - The tile of firings while people slept, when it can't say whether they
    caught anything, says when people sleep, not that they caught nothing.
- **The pages say what to check, and name a flag only where it is to be
  set** (docs/design/ui.md, bundle 63):
  - What to look at's lines and a feed's line say what to check in
    English, Japanese and Spanish, as Korean has since 0.60.0: "if they
    mattered, write them down; if not, fix what fires them", "check its job
    is still running", "Check X still earns its place", not a question; a
    task made of a spare rule's line is "Consider retiring X".
  - An impact window of the unnamed signal is "user impact" on the pages
    (the incidents, an incident's page, search, What to look at, a rule's
    changes reviewed, a marked window's incidents, the Settings trial), and
    "found from user impact" in the incidents list, not "(--impact)" and
    "found by --impact". The MCP tools, which read what the pages show,
    say it so too; the API gives the title as it was kept, and the command
    line's report is as it was. A sentence that tells whoever runs serve
    what to set keeps its flag's name.
  - An impact window confirmed with the title its form was filled with is
    written down with the window's own title, not the page's words in the
    reader's language (a named signal's window confirmed in Korean was
    "사용자 영향 (NAME)"); a title written in its place is kept. The
    incident confirmed is shown as the window was, and an incident's
    "Change it" form, saved with its title unchanged, keeps its own.
  - lapilli.dev's icon is serve's mark, on a light round plate in the dark.

### Fixed

- **Safari and the firings list** (docs/design/ui.md, "In other
  browsers"). Safari kept a select at 27 px beside 36 px buttons, and a
  field's padding put it at 37.5 px in every browser: a one-line field and
  a select are now as tall as a button in Chromium, Firefox and Safari,
  the select still the browser's own. The firings list gave a label's
  chips the least width it could beside a rule's long name, a chip one
  letter a line and a row 1,129 px tall; its labels column is now at least
  12 rem, a rule's name breaks where it must and a chip may break after
  its `=`, and a Korean button breaks between its words: the row is 91 px.
- **Serve started again with named impact signals kept the windows of the
  one with no name as incidents with no signal** (docs/design/signals.md,
  "Step 3, as built"; docs/upgrading.md, "From `--impact` to named
  signals"). From 0.55.0 to 0.61.1, `lapilli serve` started with `--impact
  NAME=PROMQL` or `--impact-file` in place of `--impact PROMQL` left the
  windows `--impact` had found with no name: each kept every rule's
  record, as an incident written down does, so in their weeks and the two
  after each a rule an entry ties only to a signal with `record: false`
  was judged, and its firings black, where `lapilli verdict` with the
  same rules and file leaves them unjudged. The leader's first round now
  ties them to the name `impact`, once, as the Settings page does when the
  first named signal is saved there: with a signal named `impact` in
  effect they are its windows, their keys, decisions, tasks and comments
  kept, matched on its labels, and, when the ledger kept no `impact` yet,
  it is made of the one with no name, its record kept and nothing read
  again when its expression and `for` are the same; with none named
  `impact`, they are the windows of a signal no longer read, kept as
  those of a signal taken away are, which keep every rule's record still
  (give the file the old expression as `impact`, before the first start
  with this release, to have them judged as its windows, its record
  kept). The firings that could catch them are judged again; the log says
  how many windows were tied, and to what, in one line, and the Run
  history notes it (`lapilli: impact windows of the signal with no name
  tied to the signal impact`). A tie the database refuses is logged, the
  signals kept all the same and the windows judged as they are, and is
  tried again at the next round. A signal added whose name the ledger
  keeps windows of has them matched on its labels, as one whose match
  changed has. With no named signal, or no window of the one with no
  name, nothing changes; the next start does nothing more. No schema
  change.

- **A sign-in's or a CI issuer's fetch that got no answer names no other
  host.** A discovery document or a key set that redirected was said, in
  serve's log and `lapilli preflight`'s report, with where the redirect
  led, its query too; a certificate not for the host asked, with the
  names it is for, and one of an authority not trusted, with that
  authority. Each is now said by the URL asked alone: "it redirected, and
  redirects aren't followed", "its certificate isn't for the host asked",
  "its certificate is of an authority this machine doesn't trust".

## [0.61.1] - 2026-11-19

A security fix to what `lapilli preflight` hides of a cloud's errors:
an ARN in the account of `--changes-aws-role`, as the pod's own role
often is, showed its role's and session's names. And lapilli.dev can be
found, and is checked like serve's pages. No schema change: the ledger
stays at 023.

### Fixed (security)

- **`lapilli preflight` hides each ARN of an account in what a cloud
  says** (0.60.0 to 0.61.0). The report's scrubber ran first, and wrote
  what it had learned into an ARN's parts: one in the account of
  `--changes-aws-role`, as the pod's own role often is, then read
  `[--changes-aws-role's account]` and was no longer an ARN, so its role's
  name and a session's (`assumed-role/pod-role/i-0abc`) were shown, as was
  what followed in another role's ARN that begins with the role's
  (`role/example-writer/other`). So were the names in an ARN of a
  partition but `aws`, `aws-cn` and `aws-us-gov` (`aws-iso`, `aws-iso-b`,
  `aws-eusc`), and what followed a comma in a role's name (`role/a,b`) or
  a bracket in its path (`role/team(a)/name`). Each ARN is hidden whole
  now, before the scrubber, the role's own named `[--changes-aws-role]`.

### Changed

- **lapilli.dev can be found, and is checked like serve's pages.** It has a
  `sitemap.xml` of every page and a `robots.txt` naming it
  (`jekyll-sitemap`), each page a canonical link, and each docs page a
  description of its own, the words beside it in docs/README.md, where all
  had the site's. The pages workflow runs axe-core over every page the
  sitemap lists, light and dark, and at 320px (`test/a11y/site.mjs`); its
  first run found code blocks and tables that scroll sideways and couldn't
  be reached by the keyboard (16 pages), and each now takes the focus.

## [0.61.0] - 2026-11-19

Pull request backtests through `lapilli serve`: a CI job sends a pull
request's rules to serve, which replays them with its own access to the
history and judges them by its record, so CI holds no metric credentials;
the backtest recipes and the action send them so. A report's `firings`
section, and the posts to the webhook signed (`--notify-webhook-secret-file`).
The rules list and the Korean as 0.60.0 read on a real deployment.

Before upgrading:
- **Ledger schema 023**, after 022, even without `--backtest-listen`:
  keep a copy of the ledger's disk, or of its database, first
  ([Going back after a schema upgrade](charts/lapilli/README.md#going-back-after-a-schema-upgrade)).
- The backtest port opens only with `--backtest-listen`, which needs
  `--backtest-trust` (who may ask): without them, serve listens as
  before.

### Added

- **Pull request backtests through serve, by the CI's ID token**
  (docs/design/backtests.md, step 1; docs/serve.md, "Pull request
  backtests"). `--backtest-listen ADDR` is a port of its own: a CI job
  sends a pull request's rules before and after, and the replica that
  leads replays them with serve's access to the history, judged by its
  record, and gives back the Markdown and JSON `lapilli verdict --compare
  --changed-only --markdown` and `--json` write, the same summary line,
  its heading saying serve's record judged it; the job posts it. No
  metric credential in CI. Who may ask is `--backtest-trust`, read at
  start, serve refusing to start on a file it can't use: GitHub Actions',
  GitLab CI's and Forgejo Actions' ID tokens, each by its issuer's keys,
  by an algorithm of a key pair, for the entry's audience alone (a CI's
  default audience and the clouds' token exchanges' refused), issued
  within the hour, with the repository's id pinned (a list of them, one
  enough, never an owner whole) and the event (`pull_request` or `push`,
  never one a fork runs in the repository's context); or a plain token,
  32 characters at least. A token starts one backtest, with the body it
  came with. Prometheus-format rules alone, 50 that differ at most, each
  selector of one metric by its name; the request 4 MiB at most, read
  only once its token verified, its entry's hour taken and the queue's
  room for it seen, a token that started one refused unread; what a rule
  reads back, its ranges, subqueries and offsets added up, 8 days at most,
  a subquery's step written in it, and no `@`. While the lane runs
  a backtest, its share of `--max-rate` (`--backtest-share`, 0.25 by
  default, 0.05 to 0.75) is kept for it, the runs asking at the rest, so
  that the two never pass the rate together; `--backtest-queue` (20), 3
  of an entry, one of a pull request its token names, a newer commit's
  superseding the one waiting; an entry's `hourly` from a bucket the
  replicas share; `--backtest-timeout` (15m), one that couldn't finish in
  it refused before it waits, saying what period would fit. The comment
  holds no label of a series a rule read, no word of what the history
  answered, nothing of where it is, and the incidents caught differently
  by their ids, their titles only for an entry with `incident_titles:
  true`; the log
  a line a backtest, with counts. The Run history lists each, and
  `/metrics` has `lapilli_serve_backtests_total{trust,result}` and
  `lapilli_serve_backtests_waiting{trust}`. Without `--backtest-listen`,
  nothing of it. The chart's `serve.backtest`. **Ledger schema 023**,
  after 022: `backtests`, a row a backtest asked, kept a day after it
  ends and 200 at most, and `backtest_tokens`, the tokens that started
  one, kept past their hour. A lapilli before this one doesn't open a
  ledger it has migrated: keep a copy of its disk, or of its database,
  first ([Going back after a schema upgrade](charts/lapilli/README.md#going-back-after-a-schema-upgrade)).
- **The backtest recipes through lapilli serve** (docs/design/backtests.md,
  step 1; recipes/backtest/README.md, "Through lapilli serve"). With
  `LAPILLI_SERVE`, the URL of serve's backtest port, a recipe installs and
  runs nothing: it reads both commits' rules from git as before (`RENDER`
  run in the job), sends them and the expectations to serve, waits for
  its report (`SERVE_WAIT`, 30 minutes; cancelled past it), and posts it
  as its own, the gates reading its summary line as they read Lapilli's:
  that line as serve wrote it, or no report, and of the rest only the
  lines Lapilli writes, any other (a GitLab quick action, a mention, a
  link, HTML, a line inside the HTML block a `<details>` tag starts) left
  out, saying so. The token is the CI's ID token
  (`LAPILLI_ID_TOKEN`: GitLab's `id_tokens:`), one `github.sh` and
  `gitea.sh` ask GitHub's or Forgejo's Actions for
  (`LAPILLI_SERVE_AUDIENCE`: `LAPILLI_SERVE`'s URL, another only with
  `SERVE_AUDIENCE_ANY=1`, and never a cloud's audience nor the CI's
  default; masked in GitHub Actions' log), or a plain token
  (`LAPILLI_SERVE_TOKEN_FILE`), each given to curl on stdin. What serve
  decides is refused with it: `INCIDENTS`, `IMPACT`, `MATCH`, and every
  argument but `--since` (whole days, 1 to 14), `--tz` and
  `--rules-select`. The URL is https, or plain http, through no proxy, to
  this machine or to a Service in the cluster
  (`FULLNAME-backtest.NAMESPACE.svc`). The action's
  `serve-url`, `audience`, `audience-any` and `serve-wait`, with
  `permissions: id-token: write`; without `serve-url`, the action is as it
  was.
- **A report's `firings` section: the span's firings one by one**, for a
  receiver that makes something of its own of each, a review item for each
  alert that keeps coming back, say. It is picked as the others are: on the
  Reports page, in `/api/v1/reports` and in `serve.config`'s `reports:`.
  `report.ready`'s `numbers.firings` is `items`, each firing that started in
  the span as `/api/v1/report`'s `firings[]` has it, with its rule's
  `severity` and `team` as the CSV's columns have them, in order of their
  start, then of their rule; `total`; and `truncated`, true when it was cut:
  at 5,000, which are all it reads whole (the rest are counted), or at 1
  MiB of `items`, whichever comes first, as receivers often cap a post at
  a few MiB and one refused is tried again for about eight hours, then
  dropped. A team's
  report lists its rules' firings alone, judged by the incidents it owns.
  Its line in the text says how many, and that they were cut; the names and
  labels in `items` are as written, as every field that names a thing is.
  No schema change: a report's sections are kept as a list already.
- **The posts to the webhook, signed**, with `--notify-webhook-secret-file
  FILE` beside `--notify-webhook-file` (the chart's
  `serve.notify.webhookSecret.signingKey`, a key of the same Secret): every
  event, each try signed again at its own time, with `X-Lapilli-Timestamp`,
  the Unix seconds it is sent at, and `X-Lapilli-Signature`, `sha256=` and
  the HMAC-SHA256 of the timestamp, a `.` and the bytes sent, in lowercase
  hex. The key is what the file holds less one line break at its end, 32
  bytes at least, read for each post so that it can be rotated, and never
  shown or logged, nor are the signatures. `serve` refuses to start with a
  key shorter, one it can't read, or no webhook to sign for; a post whose
  key can't be read later fails, and is tried again, never sent unsigned.
  `lapilli preflight` reads it with the other secrets. docs/serve.md,
  "Signed posts to the webhook", says how a receiver checks a post: the
  HMAC worked out again over the body as it came, compared in constant
  time, and a timestamp more than five minutes off refused, as Lapilli
  takes Grafana's. Without the flag, a post is as it was.

### Changed

- **The rules list and the Korean, as 0.60.0 read on a real deployment** (docs/design/ui.md,
  bundle 59): the rules list's line of notes under a row no longer widens
  the columns it spans, so the rule's column keeps its room (at 1280 px,
  on the demo ledger, 396 px for 323); a label chip breaks after its
  "=", not inside a word. Korean says 규칙 for a rule everywhere, not 룰
  (룰러 stays), and 자는 시간 오탐 in the digests, not 자는 동안 깨운 or
  헛알림. What to look at's line for no record no longer says why again,
  as the notice above it does.

### Fixed

- **`lapilli verdict --compare` with `--rules-select` keeping nothing on
  one side** exited 1, `no document of rules has the labels asked for`.
  That side is now one with no rules, as an empty directory is: the rules
  the other holds are added, or taken away. lapilli serve's backtests take
  it so. A selector that keeps nothing, with no rule left on either side,
  as a label's name mistyped makes it, is an error still, the other side
  empty or not.
- **An expectations file is read as a rule file is** (`--expect`,
  `--expect-before`): its values and what its aliases expand to bounded
  before it is decoded, in time linear in it. One denser than a rule
  file may be is refused, saying so.

## [0.60.0] - 2026-11-18

Two security fixes, `lapilli preflight` asking what the credentials that
write may write, and the pages of `lapilli serve` plainer. A backslash
before a `|` in a rule's name could end a cell of the pull request
comment's tables, on a host whose Markdown reads it so; and an RSA key
under 2048 bits verified a sign-in's or an SNS message's signature. No
schema change: the ledger stays at 022.

### Fixed (security)

- **A backslash before a `|` in a rule's name could end a cell of the pull
  request comment's tables** (affected: 0.32.0 to 0.59.0, on a host whose
  Markdown reads `\\|` as a backslash and the cell's end: comrak 1.0.0, as
  tested). `lapilli verdict --compare --markdown` wrote each `|` of a name
  in a table as `\|` and a backslash before it as it was, so
  `x\|@all [x](https://evil.example)` was written `x\\|@all …`; read so,
  what followed was the next cell, outside the code span, and its mention,
  link and HTML were read as Markdown. A backslash before a `|` is shown as
  `\u{5c}` now, so that each `|` in a table has exactly one backslash
  before it, which every host reads as a `|` in the cell. GitHub's
  cmark-gfm, goldmark 1.8.7 (Gitea, Forgejo), markdown-it 14 and comrak
  0.59 (GitLab's) read the old text as one cell, as tested.

### Added

- **`lapilli preflight` asks what Grafana's token and each cloud's
  identity may write, without writing** (docs/design/preflight.md, step
  2; open question 3, decided by the maintainer on 2026-11-18: ask all
  four clouds). With `--changes`:
  - `grafana.write` reads Grafana's list of the token's own actions,
    `GET /api/access-control/user/permissions`, which Grafana OSS 12.4.15
    and 13.3.1 serve, as measured. It is ok when the token may read and
    write every rule (an Admin's `alert.provisioning:write`), or write in
    every folder (an empty scope covers none, as Grafana's evaluator has
    it); unknown when it writes folder by folder (an Editor's); a problem
    naming what it lacks (a Viewer's), or when Grafana doesn't take it.
  - `changes.aws` asks IAM's policy simulator, as the role, whether its
    policies allow `cloudwatch:PutMetricAlarm`, `DisableAlarmActions`,
    `EnableAlarmActions`, `DeleteAlarms` and `TagResource`, and
    `aps:PutRuleGroupsNamespace`, on what `--changes-in` names: a name
    given whole as its ARN, a prefix as each alarm or namespace read
    under it, with the region as `aws:RequestedRegion`. A decision with a
    context key the simulation lacks, a prefix with nothing read under
    it, and an SCP's conditions on other keys are never an ok. It needs
    `iam:SimulatePrincipalPolicy` on the role's own ARN, which serve never
    needs: without it, unknown, never a problem.
  - `changes.azure` reads, at each resource group `--changes-in` names (a
    rule group named whole, at itself), the identity's permissions, each
    entry's `actions` less its `notActions`, wildcards included, for the
    writes of the alert rule types that may be named there; the deny
    assignments at or above it, any that may refuse a write unknown; and
    the locks there and on the subscription, a `ReadOnly` one a problem, a
    `CanNotDelete` one where a metric alert may be deleted a problem; the
    deny assignments at each alert rule read, below the group (a
    deployment stack's); and read on what each alert rule read names (its
    targets, its action groups), which Azure Monitor asks for as a metric
    alert is written. What it doesn't take, a subscription-wide target
    say, is unknown.
  - `changes.google` asks Cloud Resource Manager's `testIamPermissions` on
    each project `--changes-in` names a policy of, as the service account,
    or as the pod's own identity without one, for the alerting policies'
    create, update and delete. Deny policies, principal access boundaries
    and VPC Service Controls aren't judged, as an ok says.
  - `changes.oci` reads the user's ACTIVE groups and, for each alarm
    `--changes-in` names, the ACTIVE policies of its compartment, of its
    metric's compartment and of each above them, and judges their
    statements for `manage alarms` where the alarm is and `read metrics`
    where its metric is. A statement with a condition, or one not
    understood, is never a grant: "unknown: a policy with a condition";
    nor is a compartment named by its own name in a policy attached to
    it. Reading them takes `inspect` of groups, users, compartments and
    policies in the tenancy.

  Every new request reads, or, IAM's simulation and `testIamPermissions`,
  only answers; the allowlist holds each to its path and query, and IAM's
  to `Action=SimulatePrincipalPolicy` of the role, with the region alone.
  Nothing is written. No schema change.
- **Eight pages more of docs, and the docs in the wiki too.** Getting
  started, Words, Questions and When something goes wrong; and four of
  reference: Commands, every command's `-h` as it prints it, written from
  the help itself; The HTTP API, every route of `/api/v1` and of the
  notifications port, and the role each needs; Metrics, every metric
  `/metrics` exposes; and Upgrading, which version added each schema of
  the ledger. A test holds each to the code's list (an option, a route, a
  metric or a schema added without its line, or a line left after it is
  gone, fails), not what the page says of each. The GitHub wiki is now a
  copy of docs/, written again by `.github/workflows/wiki.yml` on each
  change to main, its pages named by their titles; the pages are still
  changed in docs/ alone.

### Changed

- **The pages, plainer: one shape for every control, fewer words, the
  logo's mark** (docs/design/ui.md, bundle 59), from using them on a real
  deployment at 1070 px:
  - A field, a select and a button are one height and one corner, and
    none is a pill: the main action filled, one beside it outlined, a way
    elsewhere a link, with no arrow after it. The team, period and search
    bar is a toolbar on the page, not a card of bordered controls; a
    field's name stands before it. A rule's tabs and the lists' orders are
    underlined by a bar. Cards have no shadow.
  - The overview's What to look at is under the tiles, before the charts,
    a line each. Refired's change from the week before, and a rule's
    firings beside the span before on its page, are no longer called worse
    or better: more may be an alert that really went off again. Precision by week with no week judged is a line saying so,
    not an empty grid.
  - The rules list's numbers are alone in their cells, and what they come
    with is a line under the row, each note saying what it is of: a row
    was up to fifteen lines tall. Firings not judged have a column of
    their own, "? Not judged", where there are any, and a row with some
    says its firings asleep are of those that "caught nothing or not
    judged".
  - A rule's five numbers are in one row from 960 px, never four and one.
  - No record is said in a line, once a page ("No incident written down
    for … : precision isn't known, and no rule is called noisy."), its why
    behind "Why". The overview no longer explains ○ and ●, the rules list
    its order, the Precision chart its name, or an empty list how to fill
    it.
  - The header and the browser's tab have the logo's micro-mark, on a
    light plate in dark mode, in place of the two circles.
  - Korean says 정탐 and 오탐 everywhere for caught something and caught
    nothing, and 백테스트 for a replay; Woken for nothing is 자는 시간 오탐,
    What to look at 검토 대상, Cleanup 남은 기록 정리. Where a count may
    hold firings not judged, Korean says 인시던트를 잡지 못한 발화, not 오탐.

- **lapilli.dev says what came since 0.37**: a Get started button; the
  status, as the README says it; `lapilli preflight`; `--expect`,
  `--check-content` and `--redact`; marked windows, goals, named signals of
  user impact and declared configuration among serve's parts; the ledger in
  Postgres with several replicas; Loki's rules; that a firing with no record
  isn't judged; immutable releases and the Sigstore bundles; and the pages'
  accessibility target. The sample report there is in the order and form
  `lapilli verdict` writes it now (the incidents' `LASTED`, the zone's
  offset, what `--impact-for` can't be sure of), and so is the README's, up
  to the near misses it leaves out.

- **An RSA key under 2048 bits verifies no signature**: one in an identity
  provider's JWKS (`--oidc-issuer`'s, `--proxy-jwt-jwks`) isn't taken: a
  set of such keys alone is refused saying so, and so is a token one of
  them signed, beside longer keys; an SNS signing certificate's key under
  it proves no message (403). Go takes 1024; NIST's floor through 2030 is
  2048. A load balancer's keys are P-256, as before.
- **`changes.cloud` is ok once the identity is taken.** It was unknown, as
  what the identity may write wasn't asked; that is now each cloud's item.
- **`lapilli preflight` redacts what the URLs of `--alarms` and `--ruler`
  name inside them**: an OCI compartment's OCID, an Azure subscription and
  resource group, an Amazon workspace, a Google project, where an error
  repeats the URL with more beside it, which 0.53.0 to 0.59.0 printed;
  and, in what a cloud says, an identity's that couldn't be taken among
  it, any other ARN of an account, GUID, or number of ten digits or more,
  or written `1234-5678-9012` (the pod's own role, an Azure object id, a
  Google project's number).
- **`lapilli preflight`'s allowlist refuses a form or a query that
  doesn't read whole** (a `;` between its pairs), which was read without
  the pair, and the rest judged.
- The chart README's Azure roles have the reads `lapilli preflight` asks
  (`Microsoft.Authorization/permissions/read`, `denyAssignments/read`,
  `locks/read`) and `Microsoft.Insights/actionGroups/read`, with read on
  each alert rule's targets, which Azure Monitor asks for as one is
  written.
- **Going back after a schema upgrade, step by step** (the chart's
  README). The changelog said, of each release that changed the ledger's
  schema, to take a snapshot first, to go back to, but not how to go
  back, and the plain way doesn't work: the release before refuses the
  upgraded ledger, and a claim deleted to be made from the snapshot under
  its own name comes back empty from a GitOps tool that heals. The steps:
  the copy, waited for (a `VolumeSnapshot`, the cloud's snapshot, or the
  files with serve stopped; a database backup); the upgraded claim's disk
  kept with `Retain` first; the copy as a claim (or a database) of its
  own name; then the release before and `serve.ledger.existingClaim` (or
  `serve.ledger.postgres.secret.name` with one replica) in one change,
  never `helm rollback`, whose old values mount the upgraded ledger; and
  what is lost and kept. docs/serve.md points to it, and CONTRIBUTING
  says a schema change's entry does.

### Fixed

- **Google Cloud's alerting policies written as the pod's own identity**
  (`--changes` without `--changes-google-service-account`) were sent with
  the reads' token, of the `monitoring.read` scope, which every write is
  refused with, since 0.24.0, whose help says "without it, the pod's
  identity writes". serve now makes the pod's write credential of its
  own, of the `monitoring` scope.

### Before you upgrade

- An identity provider (`--oidc-issuer`, `--proxy-jwt-jwks`) whose JWKS
  holds only RSA keys under 2048 bits is refused, saying so, and no one
  signs in; one with a longer key beside them signs in with that one.
  Google's, Microsoft Entra's and Google IAP's keys, read on 2026-11-18,
  are RSA of 2048 bits or P-256.
- An SNS signing certificate whose RSA key is under 2048 bits proves no
  message (403).

## [0.59.0] - 2026-11-18

The docs as pages of their own, at lapilli.dev/docs, and the pages of
`lapilli serve` as they read in use on a real deployment: the incidents
list named a noisy rule beside another rule's lead, Korean read "caught"
as noise, and a few pages said more than they needed to. No schema change:
the ledger stays at 022.

### Fixed

- **The incidents list named a noisy rule as the catch** (0.31.0 to
  0.58.0). When a noisy rule fired first and one that isn't noisy after
  it, the Caught column named the noisy rule, while the lead and the state
  beside it were the other's. It now names the rule they rest on, the
  first that isn't noisy that week; a noisy rule that fired before it is
  said apart, beneath. An incident's page marks each rule that caught it
  and was noisy that week, and a rule's page lists the incidents it caught
  by the same rule, a team picked or not.
- **Korean and Japanese said the rules list's refired weeks backwards**
  (0.31.0 to 0.58.0): "of 0 whole weeks, 3" for 3 of them, where 0 had one.
- **The overview's table of the weeks was wider than its box** in Spanish
  on a laptop's screen: a long tag in its first column now wraps.

### Changed

- **The docs are pages of their own**, in docs/ and at
  [lapilli.dev/docs](https://lapilli.dev/docs/). The README's long
  sections, "How it judges" to "What it doesn't do yet", are a page each
  in docs/, beside docs/numbers.md, listed in docs/README.md; the README
  keeps what Lapilli is, the list of the pages, and how to install it.
  Nothing in them was rewritten but the few words that leaned on the
  README's order ("below", "the tables above"), now links. lapilli.dev
  publishes the same pages, with the pages beside each, and a pull request
  that changes them builds the site before it is merged. A test holds
  every page to both lists, and every link of the README, the docs, the
  chart's README and the recipes' to a file or a heading that is there.
  The help of `--markdown` and `--on-call` names the page instead of the
  README.
- **Korean says 정탐 and 탐지, not 잡음.** 잡음 alone reads as noise. The
  columns of firings say ○ 정탐 and ● 오탐, as the overview's opening names
  them; the incidents list's column is 탐지한 규칙, and an incident caught
  is ✓ 탐지.
- **A rule's page says once why its precision isn't known.** With no
  record for the weeks it fired, a notice above its numbers says why, with
  where to write incidents down, and the card says only "not known: no
  record", as the overview has it.
- **Proposals.** With none, the page offers no filters; a team with none
  says so, with a link to every team's. With none waiting or under way, it
  says so, with a link to the closed ones and nothing to clear.
- **Goals.** The page no longer names the API it reads; with no goal
  declared, it shows two as an example, with a link to every number a goal
  can be held to.
- **Settings.** A named signal's choice to follow the match in effect says
  what that match is.
- **Sign out** is a quiet button, no longer the boldest on the page.

## [0.58.0] - 2026-11-18

Named impact signals on the Settings page, step 3 of
docs/design/signals.md: what step 2 (0.55.0) put on the command line, an
admin now keeps, tries and reads again in the UI. No schema change: the
ledger stays at 022, and a deployment with no named signal sees nothing
change but one new control for an admin.

### Added

- **Named impact signals on the Settings page** (docs/design/signals.md,
  step 3). Incident criteria lists the signals in effect, a row each, with
  what `lapilli serve` was started with of its name, who changed it and
  when, and "back to the start"; an admin adds a signal, changes one,
  renames one in place (its windows, their keys, what was decided of them
  and their tasks and comments renamed with it, the rule entries that name
  it too) or takes one away, eight at most, each bounded as the command
  line's is. The UI's signal is kept over the command line's of the same
  name. The rule entries are one list, saved as YAML in place of the
  start's as a whole, 64 at most. A signal added or given a new expression
  or `for` is read over the last four weeks before it is saved; every
  change is kept with the same bits, ties and judging again as the command
  line's, in one write, and noted in the activity log with its before and
  after. "Try it on the last four weeks" reads only the signals the change
  gives a new criterion, and what one given a new match reads by its
  labels, and shows each signal's windows in effect and with it, and the
  verdict in effect and with all of them, saving nothing. "Read it again
  over the last four weeks" reads one signal alone, counted among an
  admin's twelve reads an hour. The Incident sources panel gives a row a
  signal: when it was last read, its windows in four weeks, failing with
  Prometheus's error, where each selector's data begins, and from when it
  keeps the record. `/settings.yaml` gives the signals and the entries as
  the chart's `impact` mapping once the Settings page changed any.
  Waiting proposals are held by a change here as by one on the command
  line. No schema change: what the UI sets is kept in `settings`, as the
  impact signal with no name's is.

### Changed

- **The first named signal saved in the UI**, beside an impact signal with
  no name in effect, makes that one the named signal `impact`, its windows
  and their keys kept (docs/design/signals.md, "Open questions"); the
  last taken away gives it back, its record standing. A deployment with
  no named signal reads, judges and shows everything as before; its
  admins see "Add a named impact signal" on the Settings page.

### Fixed

- **A named signal's `match` changed now judges its windows on it**
  (since 0.55.0). Each window kept the labels it was found with, which
  judging read before the signal's, so a change of `--impact-match` or
  the chart's `match` judged every firing again and changed nothing. The
  windows' are now changed with it, in the same write; a confirmation's
  incident keeps the labels it was confirmed with.
- **A change of `--impact-for` alone on the Settings page is saved**
  whatever the UI's checks make of the `--impact` serve was started with
  (since 0.49.0): the form posts the expression back, and one the checks
  refuse (a range over a day) refused the change with it. Only an
  expression an admin changed is checked, of a named signal's form too.
- **`--impact-file` is read as written**: a YAML anchor or alias is
  refused, as it could repeat a value far past the file's 1 MiB, and so
  is a tag other than a string's, a list's or a mapping's (`!!binary`
  gave bytes, not text); a rule entry's labels are held as a team's are
  (64 values of up to 256 bytes a label).
- **A label's name and values are text** in a team, a quiet window and a
  rule entry: valid UTF-8, with no control character, as Prometheus's
  are. One that isn't is refused when it is written, in the UI, the API
  or `--config`, where it was kept as JSON with U+FFFD in its place.

## [0.57.0] - 2026-11-18

A security release, after three reviews of 0.55.0 from an attacker's point
of view: of what comes in to `lapilli serve` over HTTP and MCP; of the
ledger, `lapilli preflight` and what lapilli reads from files and other
systems; and of what goes out, the recipes and the supply chain. Two more
reviews of the fixes followed, whose findings are fixed in what is
described here. Each fix comes with a test that failed before it. It also
holds goals over a period (Added). No ledger schema change. Read "Before
you upgrade".

### Fixed (security)

- **A rule file could put escape sequences into `lapilli preflight`'s
  report** (affected: 0.53.0 to 0.56.0). Why a rule can't be replayed was
  printed as the rule had it: a Grafana rule whose evaluator held ESC and
  OSC could clear the screen, retitle the terminal or print a false "ok"
  line in a CI run of `preflight --offline` on a pull request's files.
  Every text of the report is now shown as it reads, in one place, so a
  new field can't miss it; the marks around what a server said hold a
  value drawn as the process starts, which no file can forge. The CLI's
  errors and warnings that carry what a server or a file said (`check`,
  `content`, cloud alarm, ruler and Loki warnings, `ledger copy`, serve's
  start) are shown the same way.
- **One impact window could take minutes to build, past `--timeout`**
  (affected: from 0.2.0). Overlapping windows were merged by looking for
  each label value among those kept: an impact expression that isn't
  aggregated, over a metric whose label values a workload sets (a path, a
  client), made one window of 100,000 values a label in 3 min 16 s, and
  nothing checked the time. Building one now stops when the run's time is
  up, and a label of more than 64 values is left out of the window, each
  value looked for among the 64 kept until then: 100,000 series of 8
  labels take 51 ms.
- **A rule's name in a pull request or an issue was Markdown** (affected:
  from 0.22.0). The pull request a change recipe opens, the task made
  from a proposal and the tracker issue made from a task wrote the rule's
  name, and a content finding's detail, as they came: a name chosen by
  whoever writes the rule's source could mention a team, show an image,
  or put a link in a description the repository's own bot wrote. Each is
  now a code span, as incident IDs were already, and so are the incident,
  team and assignee in an issue's text. A description cut to fit (a
  task's 10,000 bytes, Azure DevOps' 4,000 characters) is cut at a line's
  end, so that no span is cut open, and a name is cut to 512 bytes before
  it is one.
- **A viewer could make serve judge the last week again, as often as they
  liked** (affected: from 0.6.0). `/settings?try=` asked no role, no
  same-site request and no limit, and each try held the one turn heavy
  work takes, so a viewer, or a link from another site, could keep
  admins' tries, reports and goals waiting. A try is now an admin's, from
  this site, twelve an hour a person, as the impact criterion's are.
- **An editor's marked windows could make the `--marked` file be refused
  whole** (affected: 0.53.0 to 0.56.0). The kinds and each kind's week
  were counted for the file, `--config`, `--mark` and the UI together: an
  editor making 15 kinds, or 64 windows of one kind in a week, made the
  next file that added one be refused, its other windows with it, and the
  `--mark` expressions fail. What the UI and the API make now has its own
  share, seven kinds and 32 of a kind a week, and the rest is the file's.
- **Every reader saw the paths of the files serve was started with**
  (affected: from 0.31.0). A file that couldn't be read (`--deploys`,
  `--marked` and the others) put its path in the status line of every
  page. Paths, and where a link among them leads, are now said only to
  admins, a path taken out only where a text says it whole; others read
  "the deploys file" and the like. So are why a run failed (`/runs`, the
  overview, a tool's error to an agent), which could name a webhook's
  file, and why the ledger's database can't be reached, which could name
  its server, user and database, wherever it is said. The `--config` file's path in `/api/v1/settings` is an
  admin's too.
- **An expectations file could take gigabytes** (affected: from 0.47.0).
  `--expect` held at most 2,000 entries, but an entry any number of
  catches: one of 54,000 made 10.8 million results, 4.3 GiB, in a
  comparison of 200 rules. A file now holds 2,000 catches, and a
  comparison 100,000 expectations.
- **A long impact expression's stand-ins took time quadratic in its
  length** (affected: from 0.53.1). `--impact` with no name isn't held to
  the bounds named signals are (4,096 characters, 32 selectors), and
  reading its stand-ins took 11 s for 64 KiB. They are now read in time
  linear in the expression: 64 KiB in about a millisecond.
- **`lapilli preflight` named the ledger's Postgres user and database**
  (affected: 0.53.0 to 0.56.0), as the server's refusal does, and the lock
  holder's role and application name, though it hid the server's name.
  They are now taken out of what the server said, and the schema, which a
  user's name can be, said as the server said it; the holder is said by
  its session and when it connected.
- **The scrubber missed three forms** (affected: 0.53.0 to 0.56.0): an
  IPv6 address written without brackets, as TLS and Postgres errors write
  it, one in brackets with its zone, as Go writes it (`[fe80::1%en0]`),
  and `--gcp-project` inside a service account's email. The project is
  taken out of Google's own names only (a service account's, a workload
  identity pool's, an App Engine or Firebase app's), never another
  domain's.
- **The "read-only" ledger probe could write a file** (affected: 0.53.0 to
  0.56.0). With a SQLite ledger's log left without its index (a serve that
  stopped hard, a copied volume), the probe made a 32 KiB `-shm` beside
  it, which, made by another user, could leave serve unable to open the
  ledger; and with an empty ledger file and a log beside it, SQLite
  deleted the log. It now writes nothing in any state, reads the log,
  opens no ledger shorter than a page with a log beside it, and reads a
  linked ledger where the link leads.
- **The backtest recipe let GitHub's older `##[` commands into the job's
  log** (affected: from 0.32.0). GitHub's runner reads `##[error]`,
  `##[add-matcher]` and the like anywhere in a line, as Azure reads
  `##vso[`: a rule name in a pull request, said when the history couldn't
  be read, could add annotations or load a problem matcher. They are
  inert now.
- **A firing of an impact window's 65th value didn't agree with it**
  (affected: 0.55.0 to 0.56.0). The ledger kept a named signal's window
  with the first 64 values of a label in order, and a firing of another
  value, in the same impact, didn't catch it. A label of more values is
  now left out, as one not all the window's series have: it constrains
  nothing, and the window is caught by more firings, never by fewer, read
  whole or a piece at a time. A window's key, and the decisions on it,
  don't change.
- **Hardening.** Serve looks for where an impact signal's data begins only
  with the selectors of the expression it runs, never a selector kept
  beside a window in the ledger, and SECURITY.md says that whoever can write the ledger can make
  serve query the history and the rule sources with its tokens, carry out
  a change it holds as approved, and send the notices it holds. A
  CloudWatch alarm whose name isn't its ARN's is neither read nor
  written: the name a change is checked by is the one it acts on. Every
  `curl` the recipes and the chart's scripts run reads
  no `.curlrc` (`-q`), as the backtest recipe's did. The preflight
  estimate keeps to `--preflight-timeout`.

### Added

- **Goals over a period, in `lapilli serve`** (docs/design/goals.md, step
  3). A goal in `--config` takes `each: KIND`, a kind of marked window
  (`sale`, `launch`), and is checked for each occurrence of it that
  ended, and for the one on now as it stands, not each week: its value is
  the measure inside the occurrence, as the Marked windows page counts
  it. `no_worse_than: before`, with `by` as slack, sets it beside the same
  measure over the four whole weeks before the occurrence's first week,
  outside the kind, of `precision` and `incidents.median_lead_seconds`;
  `at_least` and `at_most` hold inside it alone. Of `firings`,
  `precision`, `refired.firings`, `incidents.missed`, `impacts.missed`,
  `incidents.noisy_only` and `incidents.median_lead_seconds`; any other
  measure with `each: KIND` refuses the file with its line, as before.
  Not known as a week is, with `no_before` when the weeks before have
  nothing to divide. `/api/v1/summary` gives each occurrence in the row of
  the week it ended in, with `each`, `occurrence`, `title`, `from`, `to`
  and, of `no_worse_than`, `by` and `before` (`schema` stays 1); the
  Goals page has a table of them, occurrence by occurrence; the
  overview's card counts those that ended since the last whole week
  began, and Make it a task makes the task of one missed; the digest,
  `digest.ready`, a report's `goals` section, `goal.missed` (once for
  each goal, team and occurrence), `--goal-tasks` and their comments,
  `/metrics` (the latest occurrence that ended, within its 500 series)
  and MCP's `summary` and `overview` say them. No schema change. Goals of
  each week read as they did, byte for byte (`TestGoalsWeeklyUnchanged`).

### Fixed

- **An occurrence's four weeks before leave out the incidents during any
  occurrence of its kind** (docs/design/marked.md, since 0.53.0). The
  Marked windows page, a window's page, the overview's tile, and the
  `marked` section of reports and the digest counted, in the weeks before
  an occurrence, the incidents during an occurrence of its kind of a
  window they didn't list: a window's page lists that window alone, and
  the others the windows meeting their weeks, not the four before. They
  now read every window meeting the weeks they count. So a window's page,
  too, now marks with `*` an occurrence counted together with another of
  its kind on one of its days, as the Marked windows page did.

### Before you upgrade

- **`/settings?try=` is an admin's**, asked from the Settings page in a
  browser: a script that calls it gets 403, or 400 without
  `Sec-Fetch-Site`. With `--read-only` there are no tries, as there are no
  tries of the impact criterion.
- **Marked windows made in the UI or the API**: seven kinds, and 32 of a
  kind a week. A ledger already past either keeps what it has; a write
  that makes a week busier past them is refused with 400, and renaming or
  shortening one isn't.
- **An impact window's label of more than 64 values is left out**, in
  `lapilli verdict` and in serve: a firing of any value of it agrees with
  the window. Before, `verdict` and serve's unnamed signal kept every
  value, and serve's named signals the first 64. A window already in the
  ledger keeps the labels it was kept with until it is read again.
- **`--backfill` is ten years (3650d) at most**, in `serve` and
  `preflight`.
- **An expectations file with more than 2,000 catches** is refused.

## [0.56.0] - 2026-11-18

### Changed

- **The pages say a firing with no record "not judged", as the report
  does, never black.** A firing that caught nothing in a week with no
  incident written down to judge it by is counted, not judged: the
  summary, the report and the rules list's precision have said so since
  0.41.0. Yet a rule's page called them "caught nothing" in its numbers
  ("20 caught nothing", read in Korean as 20 false alarms) and drew them
  solid in its 84 days; the alert history drew them as ● and "caught
  nothing" by the hour; the overview's chart and weeks table, and the rules
  list's ● column, counted them with the black. Each now has them apart,
  "? not judged", in a pale cell in the outline of caught nothing, with a
  legend and a table column only when there are some: the overview's
  chart and table, the rules list's ● column and its twelve weeks' spark,
  a rule's numbers, strip, days table and latest firings, and the alert
  history's hours and firings. The rules list is in order of the ● it
  shows, then of all that caught nothing. One team's rule, seen from its
  page in the team's pages and on to its firings, is judged by the
  incidents the team owns there too.
  Each firing is asked as the report asks it, by its week's record, or by
  the signals tied to it (`ledger.Ledger.FiringJudged`,
  `RuleSpansJudged`, `FiringStartsBy`), and a test holds every page's
  counts to the report's, day by day, with and without named signals.
- **The overview says why once.** With no record for the last whole week,
  a notice above the tiles says so, with links to where incidents come
  from and to write one down; precision, noisy rules and woken for nothing
  say only "not known: no record", where the same long sentence was on
  each. Seven or eight tiles are laid in rows of four, never six and one
  alone, and a tile's arrow doesn't wrap alone.
- **The rules list starts near the top.** For one who may use them, the
  controls for the rules picked are shown once a rule is picked, at the
  foot of the window, over the page, so that nothing in the list moves
  under the pointer, and what takes the focus stops above them; until
  then one line says what picking does.
- **Lighter empty pages.** The tasks page with no task shows no 0 · 0 · 0
  filters; a page with no team or period to pick has its search alone,
  with no card around it. The alert history's "only ● nothing" is "only
  those that caught nothing", and its line on a firing that was real names
  any that caught nothing.
- **Korean: 정탐률 for precision.** The Korean pages say 정탐률, the word an
  operations team uses, where they said 정밀도, in all 21 places.

## [0.55.0] - 2026-11-18

### Added

- **Named impact signals, in `lapilli serve`** (docs/design/signals.md,
  step 2). `lapilli serve` takes `--impact NAME=PROMQL`, up to eight,
  `--impact-for NAME=DUR`, `--impact-match`, `--impact-record` and
  `--impact-file` with rule entries, as `lapilli verdict` does since
  0.53.0; the chart's `impact`, as a mapping of `signals` and `rules`, is
  that file. The runs read each signal on its own, in turn within
  `--timeout`: each that hangs costs the others a round at most each time
  serve starts, then about one round in 24, asked again after a round,
  then two, four, up to a day of rounds; and one that fits in `--timeout`
  alone, though not beside the others, is read on its turn. One that
  fails, or finds more than 1,000 windows in a read or 2,000 in a week, is
  missed for the run, which records the rest, and a later run reads it
  from where it was missed, within a week, a piece at a time, no more at
  once than a run reads, the pieces of a turn held together to
  `--max-samples` as one range is, each turn going on a little before
  where the last ended, as runs overlap; a window found begun before where
  a read reads goes on from the one with its labels, or is new. What is
  left of a round goes to those left over for what they took before. Each
  signal's windows are its own, keyed by its name
  (`impact-probe-20261110T1404Z`), with what was decided of them, their
  tasks and comments carried over among its own; each keeps the record
  from its own start, where the data it reads begins, read again alone
  over the last four weeks when its expression or `for` changes. A firing
  is judged by the signals tied to its alert: its rule's entry's, or else
  those that could have seen it, by what each has seen. Confirming a
  window writes down an incident carrying its signal and match, caught as
  the window was, until its labels change. `/api/v1/report` gives
  `settings.impact_signals`, `settings.impact_rules`,
  `rules[].impact_rule`, `firings[].signals` and `incidents[].signal`;
  `/api/v1/summary` gives each row's `unjudged`, `impacts.by_signal` (`{}`
  with no named signal) and, with named signals, `settings.impact_signals`
  and `settings.impact_rules` (`schema` stays 1); `/metrics` gives
  `lapilli_impact_windows` (`signal`, `state`) and
  `lapilli_impact_signal_start_timestamp_seconds` (`signal`), and `signal`
  on `lapilli_incident_state` and `lapilli_incident_lead_seconds` of a
  named signal's windows; MCP's `incidents` each incident's `signal`, its
  `rule` the rule's `impact_signals` (its entry, the signals it is judged
  against, what its record waits for), and `summary` `by_signal`. A
  proposal waiting is held when any signal's criterion, its match or
  record, the rule entries or the match in effect change. The Settings
  page lists the signals as serve was started with them; changing them
  there comes in a later release. The Incidents page picks a signal's
  windows, says each one's signal and the other signals' windows on with
  it; a window's page says its signal, the rule entries that keep rules
  from it and the other signals' windows at the time, with what caught
  each, and dismisses them together; a rule's page says what it is judged
  against and what its record waits for. `lapilli preflight` reads and
  counts each signal, and the requests of each beside its reads (what it
  reads, a round, and where its data begins, once). Each run asks each
  signal one query, and one catching up a piece at a time one a piece;
  after the runs, one a selector, for what it reads (below). A named
  signal's window labels are held to 4 KB, as an incident's; the unnamed
  signal's are kept as before. With named signals, `/settings.yaml` gives
  no `impact`, and the Settings page's try of a criterion is refused. A
  match name of `--match` or the Settings page that isn't a label's name
  is never put in PromQL. **Ledger schema 022**, after 021:
  `impact_signals` (what meta kept of the impact signal, moved to its row,
  and a row a named one), `impact_rules`, `incidents.signal` and
  `incidents.match_labels`, `series.signals`, and `days`, `weeks`,
  `marked_days` and `marked_weeks` keyed by `signals` too, every row
  copied in once. A ledger with one impact signal with no name upgrades
  with its windows, keys, decisions, tasks, comments and start unchanged,
  and judges as it did. A lapilli before 0.55 doesn't open a ledger this
  one has migrated: take a snapshot of its disk, or of its database,
  before upgrading, to go back to.

### Changed

- **A named signal is tied to what it reads, not only to what it
  returned** (docs/design/signals.md, open question 6, decided
  2026-11-17). Since 0.53.0, `lapilli verdict` tied a firing to a named
  signal that had returned the firing's value of a match label: a 5xx
  share over a threshold returns something only while a service is hurt,
  so a service it measured all along, never hurt, was tied to no signal,
  fell back, and waited for the latest start of all. Now each selector the
  signal reads is counted by its match labels over the read (`count by
  (service) (http_requests_total)`), one request a selector, as where its
  data begins is looked for; one that only narrows another is counted
  through its stand-in (`http_requests_total{code!=""}` for
  `http_requests_total{code=~"5.."}`); and that is joined with what it
  returned. Label by label: a selector whose series lack a label (a gate,
  `and on() (sum(up{job="api"}) > 0)`) adds nothing for it, and a label
  none of its selectors carries (one `label_replace` makes, a probe
  target relabelled) has every value, so a firing is tied, and waits for
  that signal's start. A selector that narrows another is counted through
  that one, once. `lapilli verdict` asks one more query a selector it
  counts of each named signal; one that fails adds nothing, and the run
  says so on stderr and goes on.
- **With named signals, `no_record` means none of the firings was
  judged**, in `lapilli verdict`'s report as in `lapilli serve`'s report,
  `/metrics` and each summary row: since 0.53.0 a report said it had a
  record once any signal had started within it, though none of its
  firings was judged.
- **`/api/v1/summary`'s rows say `unjudged`, and `impacts.by_signal`**,
  for every ledger: the firings that caught nothing counted and not
  judged, as the report counts them, and each named signal's windows
  apart, `{}` with none. With named signals, a week's firings are judged by
  the signals tied to each, so `no_record` means none of a row's firings
  was judged, and each share is of those judged; with no named signal,
  nothing else changes.
- **In `lapilli serve`, a week's firings are judged by the signals tied to
  them, not all or none** (docs/numbers.md): with named signals, every
  firing of a week tied to the same signals is judged, or none; with no
  named signal, every firing of the week, as before. And windows of one
  signal that overlap are one, never two signals' windows: a team that
  splits one `or` expression into signals sees `impacts.total` rise where
  windows of different series overlapped, which were one incident before.
  The field means what it did, the impact windows judged against.
- **The chart's `impact` defaults to `null`**, so that a mapping can be
  given in its place without Helm saying it skipped one. An empty string
  still gives no `--impact`. As a mapping, a checksum of it is on the
  pod (`checksum/impact`), so that a change to it starts serve again with
  the file it reads as it starts.

## [0.54.0] - 2026-11-17

### Added

- **An alert that came after its incident is said, not only counted
  black.** A firing whose condition held in an incident, by its rule's
  `for` and how late its metrics arrive (`--late`, `query_offset`), but
  that fired once the incident was over, its labels agreeing, read as one
  more black stone and the incident as missed, the same as an alert for
  nothing. It still is black, and the incident missed by its rule, but the
  incident's row now names the rule and how long after its end it fired
  (`✗ nothing in time; after its end: LB5xx +3m`), and a line under the
  rules counts such firings, by rule. Of a rule replayed as several
  versions, the least `for` and lag of them says when its condition held.
  In `--json`: a rule's `late`, an
  incident's `fired_after`, a firing's `after`, and `held_from`, when its
  condition held, which `--firings` reads back, so a report's firings
  judged again say the same. Before, only a near miss said a rule fired
  too late, in the step after the replays that `--extras` cuts and that
  firings from `--firings` never reach. No stone, precision or number
  changes; `lapilli serve` doesn't say it yet.

### Fixed

- **The settings page sets a stand-in in code, as the selector beside it.**
  Where the impact signal's data begins, "nothing matched since …: … has
  data" and "kept from …, where … begins" named the stand-in in the
  sentence's own type, so it read as prose beside the selector set as code
  before it. It is now in `<code>` too, in every language (0.53.1).
- **The note on impact shorter than a rule fires on counts how late its
  metrics arrive.** A rule with `for: 3m`, evaluated every minute, whose
  metrics arrive 3m late (`--late`, its group's `query_offset`, or a
  Grafana query's time range ending 3m before now), sees
  impact 3m late and fires 7m into it; the note, which counted only `for`
  and one evaluation, left it out of the rules `--impact-for 6m` is too
  short for. The rule's lag is now in what it needs, and the
  note says how many of the rules it names read late metrics
  (`impact_short.late_rules` in `--json`).

## [0.53.1] - 2026-11-17

### Fixed

- **`lapilli preflight` no longer calls the caps' bound a problem.** With
  no firing read in the last day (a quiet day, or a history that keeps no
  `ALERTS`), `estimate.memory` gives the bound the caps allow, `--parallel`
  times `--max-samples` times 64 bytes, some 5 GB at the defaults; a pod's
  memory limit under that bound was a problem, though a run holds that
  much only when every read at once holds `--max-samples`. It is now
  unknown, saying the limit and the bound, and what to change to be held
  under the limit whatever a run reads. An estimate from what fired, over
  the limit, is a problem still, and `--fail-unknown` exits 1 for either.
- **An HTML page in an error is said by its title.** Where the history's
  API answers with a page, as a managed service's frontend does for a path
  it doesn't serve (`/api/v1/status/config`, `/api/v1/targets`), or as a
  proxy or a sign-in does, an error said the page's markup cut to 200
  bytes: a doctype, a style sheet. It now says `an HTML page` and the
  page's title, or with none the start of its text, without its scripts
  and styles. What the request carried to sign in is taken out after the
  page's entities are read, though white space or a character that
  doesn't show split it, and before the text is cut; of a page longer
  than is read, the text it is cut in isn't said. So in `lapilli
  preflight`'s `history.intervals`, and in the errors serve's runs and
  `lapilli verdict` say of a page. How an answer is taken (a page, too
  large, asked again, Loki's own text) is as it was.
- **`lapilli verdict --impact` claimed time it had no data for.** From
  0.41.0 to 0.53.0, an impact signal with no name kept the record from
  two weeks before the period, whether the expression's metrics had data
  there or not: a recording rule newer than the alerts judged, a metric
  added later, retention shorter than the span. Firings there that
  caught nothing written down were judged as if no user was hurt, black,
  and could make a rule noisy. It now keeps the record by the rule
  `lapilli serve` keeps it by since 0.49.0, and named signals since
  0.53.0: for each selector the expression reads, the first sample of
  any of its series, the latest of those across selectors; from there
  on, each firing judged by its own time. A selector with no data keeps
  no record (below). Before its start, a firing that caught nothing is
  counted, not judged. The text says from when it keeps the record, when
  that is within the period, or that it keeps none, and so does a period
  with no record, with what to do, in the text, `--check-content` and a
  backtest's comment; `settings.impact_from` is that start, and a new
  `settings.impact_no_data` (and `--check-content --json`'s
  `impact_no_data`) says a selector had no data, with `impact_from`
  missing. When its data begins before the period, every firing is
  judged as before, and only `impact_from` moves. A record that starts
  within the period leaves `no_record` false, its later firings judged.
  Each run asks one more query a selector, a `count()` an hour apart or
  more.
- **An impact signal whose selector only narrows another kept no record
  for a service that was never hurt.** Since 0.49.0 in `lapilli serve`,
  and 0.53.0 of named signals, a selector with no data at all kept no
  record, and one whose data began later held the record back: so
  `http_requests_total{code=~"5.."}`, whose series a client library
  makes at the first 5xx, kept a service that served no 5xx from any
  record, and one whose first 5xx came late from the weeks before it.
  Now a selector that only narrows another the expression reads (the
  same offset and @, and each of that one's matchers, its metric's name
  among them, one of its own) is read whenever the widest of those is,
  so long as the labels it narrows by are there: it begins where its
  stand-in's data does, `http_requests_total{code!=""}` beside
  `http_requests_total`, when that is before its own data or it has
  none. A first 5xx moves no start; a code label that came later, by a
  relabelling or a rename, holds the record back to where it came. A
  selector read with nothing wider beside it keeps its own start: a
  probe on a job that came later. Taken on trust, and said in
  docs/design/serve.md: beside everyone's requests, a route's that
  doesn't exist yet is read as everyone's are. Not a selector an
  `absent()` or `absent_over_time()` reads, at any depth. Each stand-in
  is one more query where the data begins is looked for, 32 at most. The
  settings page says of such a selector "nothing matched since", or
  "kept from", with its stand-in. In `lapilli serve`, a ledger that kept
  no record by such a selector looks for its stand-in at the next round,
  and keeps the record from its `impact_since`; one whose `impact_since`
  a late first 5xx already moved forward keeps it there, as it only
  moves forward.
- **Which selector holds the record back, when two begin together.** The
  settings page named the first of selectors that began at the same time
  as beginning later than the others; it names one only when another
  began earlier.

## [0.53.0] - 2026-11-17

### Added

- **A report without its names.** `lapilli verdict --redact`, and
  `/api/v1/report?redact=true`, give every name an alias, to show a
  report outside: rules `rule-01` on, label values by label
  (`service-01`), incidents `incident-01` on by when they started with
  their titles left out, teams, groups, sources, recording rules, and the
  rules and incidents of what `--expect` held them to. Why a rule
  couldn't be replayed, an expression a near miss quotes, shifts naming a
  schedule and the time zones hours were counted in are `[redacted]`.
  Numbers, severities and durations stay, and times, in UTC; so does
  whether a replay failed. Each list keeps its order, ties by alias,
  never by name or a file's order. Aliases are the output's own, in an
  order no name decides; `--redact-key FILE` (a random key, 32 hex digits
  or more) makes each a keyed hash (HMAC-SHA256), the same across outputs
  made with the key, never an unkeyed hash, which a short name would be
  found from by trying names. The text, `--json`, `--csv`, `--markdown`,
  `--metrics`, `--push` and `--slack-webhook-file`, and
  `format=csv&redact=true`, the firings quiet windows excused and the
  team column among it; stderr isn't redacted, and the run says so
  there.
- **Goals: what an operations team holds its alerting to**
  (docs/design/goals.md, steps 1 and 2). `--config` takes a new key,
  `goals:`, each a bound on one of `/api/v1/summary`'s numbers, for
  everyone, a team, or each team (`team: "*"`): `measure:
  critical.share_caught`, `at_least: 0.8`; `incidents.missed`, `at_most:
  0`; a team's `critical.firings` with its own `critical` severities. A
  goal breaking a rule (one bound of `at_least`, `at_most` and
  `no_worse_than`; `by` with `no_worse_than` alone; `critical` with a
  `critical.*` measure; a name unique across everyone's and every team's)
  refuses the file, with its line; one of a team there isn't is skipped
  and said. Each whole week, from Monday in `--tz`, is met, missed or not
  known, with why (`no_record`, `no_value`: nothing to divide,
  `not_whole`, `team_owns_by_catching`); the week under way is `open`, a
  count past its `at_most` already over so far. They show on a new page,
  Goals (under Reporting), on the overview (a card, and each goal missed
  last week first in What to look at, with Make it a task), in the weekly
  digest and `digest.ready` (`goals`), in reports (a section, `goals`),
  in each row of `/api/v1/summary` (`goals`; `schema` stays 1) and MCP's
  `summary` and `overview`. After each round the leader works out the last
  whole week and the twelve before it, from one read of thirteen weeks,
  for `/metrics`: `lapilli_goal_met`, `lapilli_goal_known`,
  `lapilli_goal_value` and `lapilli_goal_weeks_met` (`goal`, [`team`]),
  which a scrape reads and doesn't work out; replicas that follow read
  them from the ledger, where the leader keeps them when they change, not
  from the status it writes every few seconds. `goal.missed` is told once
  for each goal, team and week: a goal of each team's in one notice a
  week, naming the teams that missed it (50, and how many more). A goal
  taken out of the file and put back, or given another team, tells no
  week again. `--goal-tasks` (`serve.goalTasks`) makes a task, of kind
  `decide`, for each goal missed, 20 a run at most, the rest waiting, as
  the Run history page says; and, with it alone, says on each goal's open
  task, the button's too, as later weeks are missed again, met, or no
  longer missed, 20 comments a run at most. A value missed is cut away
  from its bound (79.9%, not 80%, of a goal of at least 80%). Goals over
  a marked window's occurrences (`each: KIND`, `no_worse_than: before`)
  are read and refused, saying why, until marked windows are kept.
  Without `goals:`, nothing changes. **Ledger schema 020**, after 019:
  `goals` and `tasks.goal`, with nothing to backfill. A lapilli before
  0.53 doesn't open a ledger this one has migrated: take a snapshot of
  its disk, or of its database, before upgrading, to go back to.

- **`lapilli preflight`: what serve will meet, before it is deployed.**
  It takes `lapilli serve`'s flags, read and refused as serve reads them
  (exit 2), and says of each item ok, a problem or unknown, with why: the
  rules read and how many can be replayed; the history answering
  `vector(1)`, signed as serve signs it, Loki's labels, Grafana's data
  sources and notifications read with its token; the files, the secrets'
  files (by path and length), the ledger's schema and what serve would do
  to it as it starts (an upgrade, after which a release before it can't
  open it); the sign-in's discovery and keys; who may change what, and
  each port. It estimates, asking nothing more, the requests of a day of
  hourly runs and of the backfill, as `replay.Plan` counts what a replay
  asks (`TestPlanIsRun` holds the two alike), how long they take at
  `--max-rate` and `--catch-up-rate`, how many nights of
  `--catch-up-hours` the backfill takes, what is asked beside the runs,
  and the memory to give the pod, from a day's count of ALERTS, or the
  caps' bound. It writes nothing and replays nothing: every request goes
  through an allowlist that refuses any other before it is sent
  (`internal/egress`, which every client of Lapilli's sends through), the
  ledger is read through `ledger.Probe` (SQLite `mode=ro`, `query_only`,
  and `immutable` with no log beside it; Postgres with every transaction
  read-only), and no rule, `--impact` nor `--guard` is evaluated. Outside
  the allowlist is what the clouds' credential chains do of their own:
  Google's metadata server (and whether it is there, off Google Cloud),
  the Azure CLI (which may write its token cache), OCI's instance and
  resource principals and OKE's workload identity (its auth service's
  POST among them), and the processes AWS's `credential_process` and
  Google's executable credentials start. `--json` gives the report, its
  ids and fields listed in docs/numbers.md; `--offline` asks nothing and
  reads no secret's file, for CI; `--fail-unknown`
  exits 1 for an unknown. Output is redacted as a pull request's backtest
  comment is. Whether a token or a cloud's identity may write
  (`--changes`) is unknown in this release: the identity is taken, and
  nothing written (docs/design/preflight.md, step 1).

- **Named impact signals, in `lapilli verdict`** (docs/design/signals.md,
  step 1). `--impact NAME=PROMQL`, up to eight, each with its own
  `--impact-for NAME=DUR`, `--impact-match NAME=LABELS` (the labels its
  windows are matched on, in place of `--match`; `NAME=` for none) and
  `--impact-record NAME=no` (reading it doesn't keep the record: payments
  failing says only that users weren't hurt that way). Or all of them in
  `--impact-file`, with rule entries: the rules whose names match, or whose
  labels have given values, are judged against the windows of the signals
  the entry names alone, on its own labels when it gives them; a "no
  backend" rule the platform team owns can catch the service teams'
  windows by `backend`. A signal's windows are its own, never merged with
  another's at the same time, ids `impact-NAME-1`. A firing is judged by
  the signals that could have seen it, those that returned its values,
  from each one's start: where the data of every selector it reads
  begins, so a probe added on Thursday holds back the firings of the
  services the probe returned, and those no signal returned. `--json` gives
  each incident's `signal`, each firing's `signals`, each rule's
  `impact_rule`, and `settings.impact_signals` and `settings.impact_rules`;
  `--metrics`, `lapilli_impact_windows` and
  `lapilli_impact_signal_start_timestamp_seconds`. `--redact` gives each
  signal's name an alias, and a rule entry's pattern is `[redacted]`. One
  `--impact` with no name judges as before, byte for byte. `lapilli serve`
  takes one unnamed signal until it keeps named ones.
- **Marked windows: a sale, a launch, a campaign** (docs/design/marked.md).
  A span of time of a kind a team names, that marks the firings and
  incidents during it and changes nothing else: no stone, no excuse, no
  record. A firing judged that starts during an occurrence of one (each
  repeat of one that repeats is one) and agrees with it, on every label
  both have, as a deploy is agreed with, is marked with its kind; one with
  no labels marks everything, a team's only its team's rules. Every count
  is read inside each kind and outside it:
  - from the marked file (`--marked FILE`, the chart's `serve.marked`),
    read again each run and owning its windows; `--config`'s
    `marked:`; the Marked windows page under Monitoring and
    `/api/v1/marked`, by editors and a team's editors; and PromQL
    expressions (`--mark KIND=PROMQL`, the chart's `serve.mark`), whose
    windows mark everything unless `--mark-match KIND=LABEL,...` keeps
    labels of their series, read each run and again over four weeks when
    the expression changes;
  - the page lists each occurrence, the latest first, with its firings
    inside the kind beside the four weeks before outside it, and the
    incidents in it; the rules list takes `during:sale` and
    `outside:sale` (`during:deploy`: the rules with firings during
    deploys); a rule's page, the incidents, Alert history (`kind=`) and
    the overview show the kinds;
  - `/api/v1/summary` gives each row's `marked`, by kind, with precision
    inside and outside and the incidents and impact windows during it with
    their median lead (`schema` stays 1); `/api/v1/report` each rule's
    `marked` and each firing's, the CSV a `marked` column at the end;
    reports a `marked` section, and the digest and `digest.ready` the
    windows on in the week; `/metrics` `lapilli_rule_firings_marked`,
    `lapilli_rule_caught_marked`, `lapilli_marked_on` and
    `lapilli_serve_marked_read_timestamp_seconds`; MCP's firings and
    summary their kinds; and a notice, `marked.kept`. A redacted report
    (`redact=true`) gives each kind an alias, `kind-01` on.

  Bounded: 15 kinds, `deploy` and `week` taken, which a team's editors
  use and don't make; 64 occurrences of a kind meeting a week and 256 in
  all, of which the UI's windows take 128 and a team's 32, refused where a
  window is kept and dropped from an expression's read; each firing set
  beside its week's occurrences alone. An expression changed waits for
  its read of the last four weeks before its runs' reads are kept. Ledger
  schema 021 adds `marked_kinds`, `marked`, `firings.marked`,
  `marked_days` and `marked_weeks`; going back takes the ledger as it
  was, so keep a copy before upgrading.

### Changed

- **A pull request backtest's comment hides a cut host too.** `--markdown`
  took the URLs, hosts and tenants out of what the history said word by
  word, after cutting it to length, so a host cut in the middle could be
  left; what is cut is now taken out as well, as `lapilli preflight` does.

- **`--impact` given twice without a name is refused**, by `lapilli
  verdict` and `lapilli serve`: the last one given was read, and the others
  dropped without a word. Name each (`--impact errors=...`), or join them
  in one expression.
- **`lapilli verdict --csv` ends with a `marked` column**, as serve's
  CSV and `/api/v1/report?format=csv` do: always there, `[]` in `lapilli
  verdict`, which keeps no marked windows. A reader that counts the
  columns sees one more.
- **docs/numbers.md**: "every firing of a week is judged, or none" is now
  "by the signals tied to it", and "windows that overlap are one" is now
  "windows of one signal that overlap are one". With no named signal,
  nothing is counted otherwise. The recount of catches and of no record
  reads the signals' own match, the rule entries and each firing's
  signals.

### Fixed

- **CSV cells as a reader sees them.** `--csv` and `format=csv` wrote a
  rule's name, a severity, a team, labels and incident ids as they came,
  and put `'` only before a cell whose first byte was `=`, `+`, `-`, `@`,
  a tab or a carriage return. A control or format character (a line
  break, a tab, a bidirectional override) is now written as `\u{...}`, as
  the text report writes it, and a backslash as `\\`, so no two names
  are written alike, in every cell and in the JSON of labels and
  incidents; and `'` goes before a cell whose first character past any
  spaces is `=`, `+`, `-` or `@`, or one of their full-width forms. An
  excused firing's row says `refired` false, as it is in no rule's
  `refired`.
- **docs/numbers.md** says the CSV has no byte order mark, that a deploy
  naming no label covers every firing, that `lapilli serve`'s re-fire can
  follow a firing a quiet window excused, and that
  `lapilli_rule_incidents_first` and `_alone` are left out with `--rule`.
  `TestNumbersRecount` now holds the numbers at their edges: a rule at
  `--noisy-precision` to the firing, a firing two weeks to the second
  after an incident ended, an incident only a noisy rule caught, each set
  beside `--metrics` and the text as well as the report.

## [0.52.0] - 2026-11-16

### Changed

- **A window's or an incident's labels are summed up, by name.** A window
  found in the impact signal merges every series it overlaps, with every
  value of each label they all have: tried on the Settings page, or on its
  incident's page, its labels ran on as one line of `name=value` for each
  series, forty `instance=` in a row for forty instances down. Each name is
  now a line in the try's table, one after another in the incident's
  sentence: a name with one value as `name=value`, one with more as its
  values, sorted, and past three how many it has and the first three. A
  name or a value is shown as a rule's labels are, a character that would
  turn the text around it taken out. When any is cut, a details
  lists every `name=value`, one a line, the first 200 and how many more.
  The cell stays narrow and wraps. The incident's form still changes its
  labels as one line; how windows are found, merged and matched, the API
  and what is kept are as they were. A firing's and a rule's labels, each
  a series', are shown as before.

## [0.51.0] - 2026-11-16

### Added

- **The steps after the replays say where they are, and can be bounded.**
  `lapilli verdict` replays the rules, then tries a longer `for` for the
  noisy ones, looks for near misses and reads what fired with no record:
  questions of the history again, as long as the replays at times, while
  the progress line still said only "61 of 61 rules replayed". It now says
  the step and how far it is ("then looking for rules that nearly caught the
  incidents nothing caught, 120 of at most 2000 questions"). `--extras DUR`
  bounds them, `--extras 0` skips them, and the report says so (`extras`:
  `skipped` or `cut`). A longer `for` is tried for `--parallel` rules at
  once, as the replays are, in `lapilli serve` too.
- **How long an incident lasted.** The incidents table has `LASTED`. Of
  those nothing caught that `--impact` found over several series, each
  series' own window is written under them, and kept as `spans` in
  `--json`: an incident of ten minutes may be two services' windows of five
  and ten, and the one of five is what a rule would have had to catch.
- **Impact shorter than the rules fire on, said.** With `--impact`, when
  rules judged need their condition to hold longer than `--impact-for` (their
  `for` and one evaluation; with no `for`, an evaluation), the report says how many, and what
  `--impact-for` they can catch (`impact_short`). The default `--impact-for
  5m` counts, as incidents, bursts a rule with `for: 5m` evaluated every
  minute can't catch: they read as missed. Nothing is judged otherwise.

## [0.50.0] - 2026-11-16

### Added

- **What Grafana's rules really fired, from Grafana's own state history.**
  `lapilli verdict --check-replay` and `lapilli serve`'s daily check read
  `GRAFANA_ALERTS` for Grafana's rules, which Grafana writes only once its
  alert state history is sent to Prometheus, not its default. With the
  Grafana known, they now read what its rules fired from the history it
  keeps, through its API, with the Viewer's token `--grafana-token-file`
  already holds: in annotations, Grafana's default, from Grafana 12.4 on,
  or in Loki, as its frontend settings say. `--grafana` gives the Grafana
  for the rules it reads; `--check-grafana URL` gives it for Grafana's rules
  read from files (`--rules`, `--rules-history`). A Grafana keeping its
  history in Prometheus, alone or among several places, is read from
  `GRAFANA_ALERTS` as before while it holds anything; annotations before
  12.4, which hold no labels, aren't read; `--check-series` still names
  series instead. Annotations keep labels as Grafana writes them as tags (a
  colon an underscore, spaces trimmed, one past 100 characters or past
  `tags_length` left out): the replay's are set beside them so. Measured on
  Grafana 12.4.14 and 13.3.1: every firing read from the history alike with
  `GRAFANA_ALERTS`, starting and ending at the same evaluation
  (docs/design/grafanahistory.md). A history that can't be read, holds
  nothing from before the period or the day, or tells a rule's alerts
  apart by nothing it keeps, leaves those rules not checked, and the
  others as they were; `serve` goes on to the next day, and the Run
  history page says why. What another system sends is bounded: an answer
  16 MiB, a read 5,000 requests, 256 MiB, 200,000 changes of 50,000 alerts
  and 64 MiB of their labels, each label set kept once.

### Fixed

- **A miss of what really fired is kept within bounds.** `serve` kept the
  labels of a firing found only in what really fired as the store gave
  them, and showed them on the rule's page: any length, with what turns
  text around. They are kept as a replayed alert's are, each value 256
  bytes at most and shown as it reads; and of a day's misses only the
  twenty a rule keeps are copied and written, not every one before they
  were cut to twenty.

## [0.49.0] - 2026-11-16

### Added

- **User impact in a pull request backtest.** The backtest recipes take
  `IMPACT`, a PromQL expression that returns something while users are
  hurt, given to `lapilli verdict --impact` as one argument, as it is;
  `IMPACT_FOR` (`--impact-for`); and `MATCH`, labels comma-separated, a
  `--match` each: the action's `impact`, `impact-for` and `match`, and
  every host's script alike. The expression's windows are incidents the
  rules that differ are judged by, so the comment says what a change would
  have caught or missed with no incident written down. The action's
  `args`, split at spaces, couldn't hold such an expression.

- **The numbers, defined, and counted again.** docs/numbers.md says what
  each number is as the code counts it (a stone, no record and its two
  weeks, precision, noisy, re-fired, an incident missed or caught only by
  noisy rules, impact windows, lead, the critical share caught, firings
  during deploys), and gives jq that counts each again from `lapilli
  verdict --json` or `/api/v1/report`, and `/api/v1/summary`'s from the
  report of its week. A test runs every one of them on a report and holds
  it to the report's own numbers.
- **What keeps its name.** docs/numbers.md lists the report's fields, the
  CSV's columns and the metrics, with their labels, that keep their names
  and meaning, and how one is changed when it must be: in a minor release,
  said here, the old written beside the new for a release first. Tests
  hold each (`TestReportFieldsStable`, `TestMetricsStable`,
  `TestCSVColumns`).
- **`lapilli verdict --csv` and `/api/v1/report?format=csv`.** The firings
  a row each: rule, severity, team, labels, start, end, stone, no record,
  the incidents caught, re-fired, during a deploy, excused (the CSV of
  `lapilli serve` has the firings quiet windows excused, which the JSON
  leaves out). A cell from a rule's name, a severity or a team that starts
  with `=`, `+`, `-`, `@`, a tab or a carriage return is written with `'`
  before it, so a spreadsheet doesn't run it. The API's bounds hold: 31
  days, one such report at a time.
- **The report says what judged it.** `settings` (`lead_ns`, `match`,
  `refire_ns`, `unknown_length_ns`, `record_lookback_ns`, `impact_from`,
  `record_by_week`), and each firing whether it re-fired (`refired`) and,
  in `lapilli serve`, came during a deploy (`during_deploy`): enough to
  count the numbers again from the report alone.

- **Incident criteria on the Settings page.** The impact signal
  (`--impact`, a PromQL expression that returns something while users are
  hurt) and `--impact-for` could be set only where serve started. An admin
  sets them in the UI now, in a section of their own, as the judging
  settings are set (started with, in effect, changed by, back to the
  start); empty or `off` turns the signal off, and `/settings.yaml` gives
  them as the chart's `impact` and `--impact-for`. One saved is checked
  first: as PromQL as Lapilli reads it, 4,096 characters at most, no range
  or subquery over a day nor one not written as a duration, no subquery
  stepping by less than a minute, 32 selectors at most, `--impact-for`
  from the step to a day; then read
  in full over the last four weeks, by the save or a try within half an
  hour, within 90 seconds and `--max-samples`, or nothing is saved. "Try
  it on the last four weeks" shows, saving nothing, the windows it would
  find and which would count, where the data of each selector it reads
  begins, the verdict in effect and with it (the weeks before the span
  that would lose their record, weeks with a record, incidents, firings
  judged and with no record, precision, noisy rules, incidents missed),
  and the proposals waiting for approval saving it would hold for a new
  backtest; trying the criterion in effect changes nothing. A try is an
  admin's, from the Settings page or typed, reads within 90 seconds and
  `--max-samples`, one heavy request at a time, twelve an admin an hour
  with the saves that read and the reads again saves make (in the ledger
  with replicas), and the page is kept until the ledger changes.
- **Incident sources.** The Settings page lists where incidents come from,
  and whether each is alive, in the status colours: written in Lapilli;
  each source a tool's job copies in through the incidents port, by its
  ids' prefix (`PD-` PagerDuty, a Jira project's key), with when it last
  wrote, unchanged or not, quiet after two days with nothing, the quiet
  first; the incidents file, last read or failing with why; the impact
  signal, last read with the criterion in effect, quiet when not read for
  two runs, "no data" when a selector it reads has none, with where each
  selector's data begins; each with how many incidents in four weeks, and
  the record gone quiet. Not set up is said in no colour, with a link to
  the recipes. No token is shown; the port, the token's file and the
  incidents file's path, in its error and in the status banner too, to an
  admin alone. The Incidents page links to it.

### Changed

- **The action installs the release it is at.** Its `version` is
  optional: without it, the action installs the first release the
  `CHANGELOG.md` of its own commit names, read offline, so that Dependabot
  or Renovate moving the `uses:` line moves the binary too. At a commit
  between two releases, that is the one before it. A `sha256` pinned is
  one release's: once the `uses:` line moves, nothing is installed, and
  the comment says so, until it is changed too. The release workflow
  checks that the first release `CHANGELOG.md` names is the one tagged.

- **The impact signal's record follows one rule: it holds only for weeks
  that were read, with data, by the criterion in effect, and, after a
  change, replaced by a successful read of that criterion.** The
  criterion the record is kept by (expression and `--impact-for`) is kept
  beside `impact_since`, with what the last run read. A new one, saved in
  the UI or started with, is read again over the last four weeks by the
  replica that leads: its windows take the place of those found from then
  on, the record is kept by it from then, and the weeks are judged again,
  a week a write. Until that read succeeds, a run that reads it keeps a
  floor at its start, and no record is kept by the signal at all, before
  the floor either (the record has one start, and the rule judging weeks
  is unchanged): more saves meanwhile leave the floor where it is, and
  saving the one kept back still needs it read again. No record is kept
  by the signal before where its data begins is looked for. A decision
  about a window found before carries over to a new one it covers half of
  or more, of the new one (the one covering most, confirmed before
  dismissed on a tie); a key, and the tasks and comments on it, need half
  of both windows: a window keeps the key of one found before only when it
  is the same one, so nothing kept on one lands on another, and the tasks
  and comments on a window no longer there move to the one covering most
  of it, sharing labels and half of each, or are listed on the Cleanup
  page; a window starting in the same minute as one kept is kept too. The
  weeks before keep only the windows the one before found, as incidents.
  One that fails to be read again is tried again later, up to a day
  apart. A ledger kept by an earlier lapilli is taken to have been read
  with the command line's criterion, and another saved is read again. A
  backtest keeps the criterion whose windows it was judged against, a
  label of its own while a floor stands, and a proposal waiting for
  approval is held for a new backtest once another is read in their
  place. No schema change; `verdict.Semantics` is unchanged. The
  overview's weekly panels the digest works out catch up at the next
  digest.

### Fixed

- **The impact signal claimed weeks it had no data for.** From 0.41.0 to
  0.48.0, `lapilli serve --impact` kept the record of every week from
  where its runs first read the signal, whether the expression's metrics
  had data there or not: a recording rule newer than the alerts judged, a
  metric added later, retention shorter than the span. Firings in those
  weeks were judged as if no user was hurt, black when no incident was
  written down, and proposals backtested over them held. Now, for each
  selector the expression reads, the first sample of any of its series is
  found; the record starts no earlier than the first whole week after the
  latest of them, as only there did every selector have data, and not at
  all while one has none. On upgrade, a ledger's `impact_since` moves
  forward to it at the first round that runs, never back, and no record
  is kept by the signal until it is looked for; a run that replays from
  before where it was looked for has it looked for again from there; the
  weeks before have no record unless an incident keeps them. This looks for where data
  begins, per selector (with its offset or @), not for gaps after. An
  expression that reads no selector (`vector(1)`) has data all along.

## [0.48.0] - 2026-11-16

### Added

- **Jira's incidents from the fields a team keeps them in.** `jira.sh`
  reads an incident's start and end from the fields `JIRA_START_FIELD` and
  `JIRA_END_FIELD` name (`created` and `resolutiondate`, as before, by
  default), a custom field by its id, several tried in turn; and its labels
  from the fields `JIRA_LABELS` maps (`service=components` by default):
  texts, numbers, lists, options (a cascading select's parent), components
  and users' display names. It asks Jira for those fields alone. A time is
  one Go reads as RFC 3339, with a real day, hour and zone. A date, alone or
  with something after it that isn't a time, is never taken for one: an
  issue whose start is one is skipped, unless a later field in the list
  holds a time on that day (`customfield_10001,created`), as Jira writes it
  in the account's time zone. A start holding anything else is skipped too.
  An end that can't be read, or is before the start, is left out, and the
  incident taken to last an hour. Label values serve would refuse an
  incident for are left out: over 256 bytes, past 64 a label, past 4 KB in
  all; `JIRA_LABELS` names 32 labels at most. Each issue skipped or changed
  is said on stderr by its key, never its title, with a count by why. When
  every issue found is skipped, the script writes nothing and exits 1, so a
  cron job shows it. The ids are still the issues' keys.
- **`lapilli serve` keeps off the quota of the history it shares with the
  alerts** ([docs/design/quota.md](docs/design/quota.md)). Each part is off
  by default, and `--max-rate`, `--max-samples`, `--timeout` and the 429
  retries are as they were.
  - `/metrics` counts what each serve process asks of each history API, `api`
    one of `prometheus`, `loki` and `grafana` (its data source proxy):
    `lapilli_serve_history_requests_total{api,answer}` by the class of the
    answer (2xx to 5xx, or none), `lapilli_serve_history_throttled_total`
    (HTTP 429), `lapilli_serve_history_retries_total`,
    `lapilli_serve_history_wait_seconds_total{api,reason}` (`api` or
    `rate`) and `lapilli_serve_history_yields_total`.
  - `--yield-on-429`: an API's first 429 to a run ends the run's requests
    to it, not asked again after a wait; the run stops after its piece,
    and what reads the history after it that round (reviews of changes,
    the digest and what follows it, backtests again, changes carried out,
    the replay check) waits for a later round. The rules it didn't replay
    are replayed first by a later run, from where they were missed, and the
    runs go no further than a week past the oldest of them, so none is
    given up. A 429 to `--impact` or `--alerts` holds the run, not fails
    it. `--yield-for DUR`, a day at most, leaves the API alone that long.
  - `--guard EXPR`: a PromQL expression asked before each run and each day
    of a catch-up, such as the rate of the evaluator's own evaluation
    failures; while it returns anything, or can't be read, the run waits,
    and what reads the history after it. One refused as written fails the
    run, each time.
  - `--catch-up-hours HH:MM-HH:MM`: a run more than a day behind where the
    ledger's firings are up to (the first start's `--backfill`, after being
    down that long) runs only in those hours, in `--tz`; a run hours behind,
    or replaying again from when a version of the rules took effect, runs
    at once. `--catch-up-rate N` asks each API N requests a second at most
    while catching up.
  - A run that waits is kept in the run history as `held`, with why, once
    for each reason, logged at Warn; it isn't counted as failed,
    `lapilli_serve_runs_total` counts it as `result="held"`, and
    `lapilli_serve_runs_held{reason}` says why the last run waited.

## [0.47.0] - 2026-11-16

A change to the rules is held to what they are expected to do: how often
each may fire in a week and while people slept, and the incidents each
must keep catching, written beside them. A pull request backtest says what
the change broke, and can fail the job for it; a budget needs no incident
written down. No ledger schema change; the API and MCP answer as they did.

### Added

- **Expectations in a pull request backtest.** `lapilli verdict --compare
  --expect FILE` holds the rules that differ to what is expected of them
  ([docs/design/expectations.md](docs/design/expectations.md), its first
  step): a budget of firings in each whole week of the period, and of
  those while people slept, and incidents each must keep catching, within
  how long. The report says each before → after: met, broken by this
  change, broken before too, or not known (an incident not in the period;
  hours of no sleep). `--expect-before` gives them as they were, for the
  rules in place; `--fail-expectations` exits 1 when the change breaks
  one, `--expect-required` when it adds a rule none holds. The summary
  gains `expected`, `expectations_broken`, `expectations_already_broken`
  and `unexpected`. The backtest recipes take `EXPECT`, read from both
  commits, and `FAIL_ON_EXPECTATIONS` and `EXPECT_REQUIRED` (the action's
  `expect`, `fail-on-expectations`, `expect-required`), which fail the job
  once the comment is posted, and when the expectations can't be judged (a
  rule that differs not replayed, the file unreadable, no report).
  A budget needs no incident written down. An expectation the change takes
  away is still shown, and fails nothing; a name several rules share holds
  each of them.

### Fixed

- **"None yet" where there is none.** The changes' page ("Is the record
  kept up?") showed the latest incident written down as "–" when there was
  none, in the warning's colour once two weeks had gone by: "–" is what
  Lapilli shows where it can't know, and here it knows there is none. It
  says "none yet" now, with how many days, as before.

## [0.46.0] - 2026-11-16

### Changed

- **The sidebar names what a page holds.** Its groups were verbs and some
  pages coined words, so the menu read as a list of commands; they are
  nouns now, as a console's are. The groups: Monitoring (was See), Improve
  (Fix), Reporting (Report) and Administration (Manage). The pages:
  Alert history (Firings), Run history (Runs) and Cleanup (Orphans); in
  Korean 발화 이력, 실행 이력 and 정리 대상, with Outcomes 변경 효과 (was
  효용) and in Japanese 変更の効果, and the groups 모니터링, 개선, 리포트
  and 관리. Proposals keeps its name, as everything on and beside its page
  says proposal, and Outcomes its English one: "Impact" would have stood
  beside the impact windows on the same page. A page's
  heading, breadcrumb and browser tab say the name the sidebar gives it,
  and so do the links and lines that name a page (a cloud alarm's, the
  failed digest's, the overview's Outcomes card and the Administration
  page's cards). What a page holds keeps its words: firings, runs, a
  rule's tabs. The URLs (`/firings`, `/outcomes`, `/runs`, `/orphans`,
  `/manage`), the API, MCP and JSON fields are as they were.

### Fixed

- **No change, no "better".** The Outcomes page said "+0 pt, better" of a
  precision that read 1% then and now. A change is one between the
  numbers shown, on the overview's tiles and Outcomes' then and now: the
  same number shown is neither better nor worse, with no colour and no
  word, and two that differ never read "+0 pt", "+0%" or "+0s". Outcomes'
  firings and ● a week say a change from none ("+4"), and one between the
  numbers shown (1 then, 3 now is +200%, not the unrounded rates' +86%);
  the overview's time to catch says its change in the unit its time is
  shown in (minutes from an hour on, not "+41s" beside 1h5m).
- **"Propose it" with a proposal waiting.** A rule's "What to do first"
  offered its suggested `for` while the rule had a proposal not finished
  with, which is refused: one a rule at a time. The button is disabled
  then, with why (to a viewer, as before, that editors alone propose) and
  a link to the one that waits.
- **The rule's other forms with a proposal waiting.** So are the forms
  that would be refused the same way: on the Proposals tab a new `for`, a
  threshold, what it says and retiring it, and on the summary a fix to
  what it says. To an editor each stays drawn as it is, its fields and
  button disabled, with why said once above them and a link to the one
  that waits; a viewer is told, as before, that editors alone propose,
  with the same link. Once it is finished with, the forms are back.
- **A disabled field looks it.** A field no one may fill now is drawn as a
  disabled button is: dim words on the panel and a dashed edge. So are the
  rules list's bar's select and input, disabled for a viewer, which looked
  like fields to fill.
- **A checkbox apart from its words.** In a form of fields a checkbox
  stood on a line of its own, centred over its words ("take it out
  instead", a report's sections, places and "On"); it is before them now,
  on their first line's baseline, where words that wrap leave it.

## [0.45.1] - 2026-11-16

A rule nothing could judge, as no incident was written down, could still
read "○ OK" on the rules list; a rule's page, picked from a team's list,
could say otherwise than the list did. No ledger schema change; the API and
MCP answer as they did.

### Fixed

- **A rule with no record was "OK".** A rule none of whose firings was
  judged, as no incident was written down in their weeks or the two weeks
  before, and that fired fewer times than a noisy rule does, was listed
  "○ OK" (since 0.41.0): a verdict no record could give. It is "? No record"
  now, on the rules list, its page and the facet (`perf=unrecorded`,
  `is:unrecorded`), as a rule that fired as often already was. The
  overview's panel of rules that fired often with no record lists the same
  rules as before. A rule some of whose firings were judged is judged by
  those, as before.
- **A rule's page, seen from a team's.** Picked from a team's rules list
  (`?team=`), a rule of that team's was judged on its page by everyone's
  incidents, and on the list by the team's: the list could say "? No
  record" and the page "● Noisy". Its page is judged by the team's
  incidents too now, as the list is; from another team's, or none,
  by everyone's.
- **`--max-samples` says what it bounds.** A replay reads a rule's query a
  day at a time, on days from midnight UTC, and the bound is each read's:
  a day's, some 6,900 series firing all day at a minute by default. The
  help and the README said a rule's query over the whole period. The
  test of the bound read the real clock, and failed for the six hours
  that end between 02:40 and 03:20 UTC, as they are two reads; it reads
  noon UTC now.

## [0.45.0] - 2026-11-16

The web UI says what a state is at a glance: fine, needs a look, broken,
under way, or not known. The calm palette stays as it was: the olive for
what can be done, the blue and grey of what caught something and what
didn't, the lavender of where the noise is. Five status colours are added
beside it, on small surfaces alone, and each state has the same one on
every page. No ledger schema change; the API and MCP answer as they did.

### Changed

- **Status colours.** Each state is one of five kinds: ok (a cool green,
  told apart from the olive), warn (amber), bad (red), info (the blue) and
  unknown (the dim grey). Each kind is a text colour, a wash and a line,
  in light and dark. They colour a badge or a pill, a notice's left edge
  and background, a cell, a word or a sign. They never fill a row or a
  card.
- **The same state, the same colour.** "Awaiting approval" and a stale
  proposal are amber on the proposals list and on the proposal's page
  alike. They were lavender, olive, amber or red, depending on the page.
  An approved or applied proposal and a rule that is OK are green. A
  noisy rule's badge is amber, as the lavender is kept for how much
  noise. A proposal closed as failed is red; one refused, dropped or
  changed in review is grey; one that expired is amber. The task page
  shows a proposal's state as the proposals list does, not as a grey
  word.
- **Red only for what failed.** The boxes that were all red say warn or
  bad by what they mean. A proposal an agent made, a `for` one event
  can't outlast, a backtest that didn't finish, teams whose names differ
  only in case and "no one can approve" are amber. A backtest that
  misses incidents, a run's error, a digest that failed and notices given
  up on are red. The banners above every page (changes paused, what the
  server is doing, its error) are boxes of the same kind.
- **Never green or red for what isn't known.** A rule with no record is
  grey ("? No record", amber before), as are a backtest with no record
  and the notes that say no incident was written down. A rule none of
  whose firings was judged is grey even where its badge says "OK" (it
  isn't noisy, but nothing says it's fine). A number Lapilli can't judge
  is "–" in no colour, as before.
- **Green only for all clear.** The overview's "Nothing to look at now"
  is green with a ✓. Other empty states, such as "No proposals yet.",
  have no colour. Outcomes' "all ran" is green only when the runs are up
  to date; while they are behind, it is blue.
- **The incidents chart.** Caught, caught only by noisy rules and missed
  are drawn in the same hues as the status colours, 3:1 on the card in
  light and dark. Each pair is at least 8.5 apart (ΔE in OKLab, ×100) for
  readers with protanopia or deuteranopia, and caught and missed differ
  in lightness as well as hue. Before, the amber was under 3:1 on the
  cream, and caught and missed were 4.1 apart for deuteranopia.

### Added

- **Words beside colours.** A quiet window in effect now says "active
  now". An overdue task says "overdue" beside its date. A headline
  number's change says "better" or "worse", as a rule's panel already
  did. These were said by colour alone before. The words are in all four
  languages.
- **Checks.** `TestContrast` holds every status colour's text on its
  wash, the card, the background and the panel at 4.5:1, ink, quiet text
  and links on each wash at 4.5:1, and each line and a link's underline at
  3:1, in light and dark. `TestStatusKinds` renders the pages the
  accessibility checks render, a proposal in each of its states and seven
  more deployments (no record, nothing to look at, rule changes measured,
  notices sent and failed, changes paused, and others). It finds each of
  73 states by its words on the pages named for it and holds it to
  its kind there, so a colour left out fails. It also holds every
  coloured state to one kind on every page. `TestChartFillsApart` holds
  the chart's fills apart for colour-blind readers. axe-core finds no
  violation on the pages it checks, in four languages, light and dark.

### Fixed

- **The incidents list's colour.** A missed incident coloured the Lead
  column, the last one. The column that says how it was caught is
  coloured now.

## [0.44.2] - 2026-11-16

A deployment with no incident written down had its overview read
"Woken for nothing: 21": a judgement, in a tile's number, of firings no
record could judge. Precision and noisy rules there said "not known"; the
tile beside them did not, nor did the rules list it leads to, a report's
woken section or the digest's notifications line.

### Fixed

- **Woken for nothing, with no record.** A week with no incident written
  down in it or the two weeks before (no record), in which anything came
  while people slept, has no number in the overview's "Woken for
  nothing", as it has no precision: the tile says "not known", and how
  many firings came while people slept that week, or, once notifications
  are counted, how many notifications. A week of notifications alone, for
  a firing begun before it, is judged by its record too. Its line of weeks
  leaves such weeks out. A week before the first notification still shows
  "–" for that reason, as before.
- **The rules that week.** The rules list, where the tile leads, counts a
  rule's notifications while people slept in weeks with no record apart:
  "notifications while people slept, with no incident written down to
  judge their firings by", not "for nothing".
- **Reports and the digest.** A report's woken section says "Firings (or
  Notifications) that came while people slept, with no incident written
  down to judge them by" over a span with no record, and, over a span with
  some firings judged and some not, "for nothing, or … with no incident
  written down", as the command line does; its numbers carry `no_record`
  and `unjudged`. The notifications line, in the digest and a report's
  notifications section, says the same of its notifications and names the
  rules that sent the most "for those firings", not "for nothing"
  (`no_record`, `unjudged` in its numbers; `for_nothing` and
  `most_for_nothing` keep their names).
- **Korean.** These lines no longer say 오탐 or 헛알림 of firings with no
  record, and a rule's page says its notifications were "for firings that
  caught nothing" (아무것도 못 잡은 발화), not 오탐 발화. The ● stone keeps
  its name, 오탐, as the tile keeps its own, "괜히 깨운 발화".

## [0.44.1] - 2026-11-16

A security fix: what a rules input makes beyond its own text is bounded,
for the input and for the inputs read together, where it is made; and a
rule file Lapilli refuses is said, never passed over unseen. No ledger
schema change; the API and MCP answer as they did. Read "Changed" before
you upgrade: some rule sets passed over unsaid before are an error now.

### Security

- **A small rules input could make gigabytes** (affected: every release
  from 0.1.0). Copies were made for each rule before anything looked at
  them, from whatever a rule's writer put in one place:
  - the text YAML aliases bring in again, which yaml.v3 bounds by how many
    values it is, not their text (from 0.1.0): an 8 MB string aliased by
    100 rules' labels made 5.1 GB, parsed and hashed; and the maps an
    aliased mapping makes, counted by their text alone: 4,000 short keys
    aliased by 40 rules, 41 KB, made 72 MB;
  - a Google Rules' namespace, and an Azure rule group's cluster, put into
    each selector of each rule's expression (from 0.5.0): a namespace of
    40 KB on 1,000 rules of 16 selectors, 99 KB, made 6.5 GB, parsed and
    hashed, an Azure rule group as much; and each number of such an
    expression lowered the rest of it, 431 MB for 20,000 numbers, 6 s of
    CPU for 100,000;
  - a group's labels, on each of its rules (from 0.32.0): 9,999 labels on
    6,700 recording rules, 286 KB, made 4.2 GB; on 6,700 alert rules, one
    label with a direction mark (from 0.34.1), 4.2 GB; on 3,000 recording
    rules in a ConfigMap, 193 KB, 1.9 GB; on 1,000 alert rules, 123 KB, 2.3
    GB hashed; on 60,000 alert rules, 1.8 MB, 11.7 s to read;
  - a group's name, on each of its rules, in each rule's key (from 0.4.0)
    and copied for each to be shown when it has a character that doesn't
    show (from 0.34.1): a name of 1 MiB with a direction mark on 1,000
    rules kept 1.07 GB, and one pass of their keys made more than 2.2 GB;
    a Grafana group's name of 1 MiB on 300 rules made 964 MB;
  - a ConfigMap's value that an alias makes another key's, read into rules
    again for each key (from 0.1.0): 10,000 rules by 16 keys, 250 KB, made
    1.2 GB, and past 2.2 GB through `--rules`; as `!!binary`, decoded
    afresh for each alias, 320 KB made 1.22 GB;
  - a List some of whose items are left out, written afresh with its
    aliases in full and kept so in the ledger (from 0.29.0): 69 KB was
    kept as 3.9 MB; 820 KB, bytes that aren't UTF-8 under 190 mappings
    aliased ten times, as 109 MB, having allocated 861 MB.

  What an input makes beyond its own text is charged now where it is
  made, before it is made: the larger of 4 MiB and 32 times its size, 256
  MiB at most, for the whole input (a file, a directory's files, a stream,
  a ConfigMap's files, a ruler's namespace, the documents of one source in
  a version kept in the ledger). Each input above is refused having
  allocated 1 to 121 MB, its YAML read among it, within 0.11 s; the
  expression of 20,000 numbers is scoped having allocated under 1 MB; the
  ConfigMap's recording rules are made only when asked for, and it reads
  having allocated 13 MB. A value in the shape of rules a YAML alias
  brings in is refused in a ConfigMap's `data`, naming its key, whatever its tag
  (`!!str`, `!!binary`, read as the text it decodes to) and wherever the
  alias is (the value, the data, a merge into it, the whole ConfigMap): a
  ConfigMap the API server keeps holds none, and `binaryData` isn't read
  for rules. One that isn't, by its text (an env-style value aliased by
  another key), is passed over unread, as before. So is an Amazon rule
  groups namespace's `data` an alias brings in refused before it is
  decoded, naming the namespace: each namespace aliasing it was decoded
  and read into rules again, and the AWS CLI writes none. A document
  written afresh is written into the room the input has left and charged
  what it is written in, not estimated; what is kept so, where it is read
  in the file's place (`--rules-select` leaving an item out, `lapilli
  serve` keeping only the documents of rules), is read as the file it is
  of, what it may make judged by the file's size and what the copy holds
  beyond the file charged as made, and is 16 MiB at most, as a file is
  (read as a file of its own, its size, aliases written out, set what it
  might make: a List of 41 KB written out to 1.5 MB, read with its
  recording rules, made past what the file may). A file read as it is
  isn't held to what its copy would be. A ConfigMap whose `data` an alias
  brings in is refused there too: counted broken and kept, it was written
  afresh with each alias in full and read so, a rule file again for each
  key. No label and no name is capped: Prometheus takes them of any
  length. Real sets are far inside the bound:
  kube-prometheus-stack's rules four times over, each group with labels of
  its own, make 1.8 MB of the 208 MB they may; 5,000 rules sharing six
  labels and three annotations by aliases, 10.4 MB of 20 MB.
- **A ruler's namespace could have another team's rules passed over.** From
  0.34.1 an Amazon ruler's namespaces counted their YAML aliases as one
  input, so one namespace's aliases could have a later one refused for
  excessive aliasing, passed over in its place; with the bound above
  counted the same way (as it was before its review), a namespace that
  made much within it (11 KB making 4.18 MB) did the same to any ruler's,
  the warning naming the innocent one, and a namespace refused gave back
  what it counted, so twenty refused in a row allocated 2 GB a read. Each
  namespace is an input of its own now, refused and passed over alone,
  named; what all of them make, refused ones' among it (what each made
  before it was refused, not the charge refused, which isn't made) and
  never given back, is 256 MiB at most, past which the read fails whole,
  naming the namespace whose making took it past, in whatever order they
  are read, and the one that had made the most, which may be another (what
  one makes in reading counted before it is done), and the version in
  effect stays. Twenty refused namespaces allocate 4 MB.
- **A rule file Lapilli couldn't read was passed over unsaid** (from 0.1.0).
  In a ConfigMap, a rule file that yaml.v3 decodes, as Prometheus loads
  it, but Lapilli's reader refuses (a rule of 10,001 labels, merges counted
  stricter than yaml.v3, aliases past its bounds) was taken for a value
  that isn't YAML and left out, so a pull request's rule could miss the
  backtest unseen; so was one refused for what it makes, through FromDocs,
  as serve reads a version from the ledger, and a ConfigMap holding only
  that file was "no alert rules". Only a value yaml.v3 can't decode isn't
  YAML now: one Lapilli refuses, in the shape of rules, is an error naming
  its key, and broken where a stream is picked. So is a file below a
  directory's top in that shape, as at the top: it was passed over as one
  that doesn't read is, though the document refused may be another of the
  file (a stream whose Deployment has a key Lapilli refuses, beside its
  PrometheusRule). In the shape of rules is a mapping with `groups` (a key
  as written, whatever its tag, or as its text is read, below), a `spec`
  with `groups` (a PrometheusRule, a Google `Rules`), or a merge, or a
  list one of whose items is a mapping with `groups` or a `kind`; and a
  document whose nodes don't tell (past `Nodes`, brought in by an alias,
  or another document refused in the same file) by its text: `groups`
  written, or written by double-quoted escapes (`gr\x6fups`), or
  `Z3JvdXBz`, the base64 a `!!binary` key `groups` is written in, however
  its tag is written (`!!bin%61ry`, a `%TAG` directive) and across
  whatever spaces and line breaks. A backslash or `binary` alone in a file
  of data or an env-style value isn't. A value not in that shape is passed
  over, whatever Lapilli's reader says of it (a file_sd list whose labels have a
  key twice, a list 250 deep), and a rule file with a key twice, which
  yaml.v3 refuses too, is broken, as before. A refusal for what an input
  makes is said wherever it is read: `lapilli verdict --rules` and
  `lapilli serve`'s "reading the rules" (its log and status; the version in
  effect stays) fail with it, as does reading a version in the ledger
  again, and a ruler's namespace is passed over with it, named.
- **A mapping's keys that weren't strings hid what was under them**
  (affected: every release from 0.1.0; a rule file's own keys from
  0.34.1). A mapping one of whose keys wasn't a string was read as yaml.v3
  decodes it into Go, a map no rule is read from: a ConfigMap's `data`
  with a key `0` hid every rule file in it, a rule file with a `!!binary`
  `groups` key (or any key `1`) hid its rules, and labels with a key `true`
  lost every label, unsaid. Keys are read as text now, as Prometheus
  decodes a rule file's: plain scalars as written (`0`, `true`, `1.5`, a
  date), a `!!binary` key as the text it decodes to; a key that isn't a
  scalar (as before), a null one (which Prometheus passes over and
  Kubernetes refuses), one of another tag (`!x`), and two keys of one text
  (an alias of a key beside the key, a `!!binary` key beside what it
  decodes to, which Prometheus loads, the last one set) are refused,
  naming the line, said as Lapilli's own refusals are, never passed over
  as a file that doesn't read.
- **A replay copied a rule's labels for each of its series** (from 0.1.0):
  a rule of 9,999 labels on 1,000 series made 1.3 GB in one replay. The
  copies are counted before they are made now, against what a range's
  samples are held to (`--max-samples`, 640 MB by default); past it the
  rule isn't replayed, saying so in its own words (`too many labels on
  its series to replay`), which `--fail-unreplayed` counts: a rule's
  writer could otherwise keep a rule out of a backtest unseen. So too
  wherever a replay's answer is kept or used: `lapilli serve` keeps such a
  rule's firings as they were and lists its run as failed, as for a query
  that failed (taken for a rule that can't be replayed, its firings were
  dropped and the window recorded as replayed empty, never tried again),
  and a near miss's cost and an unrecorded firing's clearance are an error
  (`rules.Unanswered`). A dozen
  labels on 50,000 series take some 100 MB.
- **What is still multiplied.** Each bound is per input: lapilli serve
  holds every version of the rules in a window at once, and keeps the
  rules of up to 128 versions read, so what one version may make is
  multiplied by how many versions there are; and each request that works
  out a rule's labels for each of its rules, at once with others, makes it
  again. Each is a version's or a request's own, bounded as above.

### Changed

- **Errors once passed over are said**, and a rule set that read before may
  be refused now, with why: a ConfigMap's rule file Lapilli can't read, a
  file below a directory's top past what the directory may make (the whole
  directory is refused) or in the shape of rules that Lapilli's reader
  refuses, a namespaced Google `Rules` whose rules don't read (before, the
  document gave no rules and no error), a ConfigMap whose `data` holds a
  value in the shape of rules a YAML alias brings in (one that isn't is
  passed over, as before), wherever a stream is picked too, and an Amazon
  rule groups namespace's `data` a YAML alias brings in.
- **A mapping's keys are read as text** in rules inputs, as Prometheus
  reads them: what was under a key `0`, `true`, `1.5` or `!!binary` (a
  ConfigMap's rule files, a rule file's groups, labels) was left out, and
  is read now. A null key, a key tagged other than YAML's own (`!x`), and
  two keys of one text (an alias of a key beside the key, which Prometheus
  loads) are refused, naming the line: in a ConfigMap's rule file, an
  error naming its key; below a directory's top, an error, as at it.
- **What a stream keeps, written afresh, is read as the file it came
  from** (a List some of whose items `--rules-select` leaves out, or
  `lapilli serve` keeps only the documents of rules of): what it may make is
  judged by the file's size, its aliases written out charged to it, and
  what is kept is 16 MiB at most. A rule set with many aliases, read with
  its recording rules (`--compare`), may be refused now where it read
  before.
- **`--fail-unreplayed` counts a rule too wide to replay** (its labels on
  each of its series past what `--max-samples` lets a replay hold), as it
  counts one whose history couldn't be read; its message says so.
- **Durations are read as Prometheus writes them**, years, weeks and days
  with the rest (`1w`, `1y`, `1d12h`), for every source; before, only a
  number of days was, and a namespaced Google `Rules` of `for: 1w` was
  refused. What Go reads is read as before (`1.5h`, `1m1h`, `1us`, which
  Prometheus doesn't), and a duration past 100 years is refused (`101y`,
  which Prometheus takes).
- **A version kept in the ledger is read again in the inputs its sources
  were read in**: `--rules`'s files together, each ruler's namespace and
  alarm on its own, a Loki's apart; two documents each read alone were
  refused read again as one.

## [0.44.0] - 2026-11-16

Teams, quiet windows, reports and the editor roles admins give were the
ledger's alone, made in the UI, so a team that runs everything through Git
couldn't review, restore or copy them; rules deleted or renamed left notes,
tasks, comments and proposals no list showed; and a proposal nobody
approved held its rule for good. Schema 019: upgrading needs nothing, a
ledger whose teams' names differ only in letter case included.

### Added

- **Declared configuration.** `lapilli serve --config FILE`
  (`serve.config` in the chart, rendered into a ConfigMap, best given as a
  text) is YAML of `teams`, `windows` (quiet windows: once, every day, week
  or month, or by cron, a team's or everyone's), `reports` (when, for
  whom, what each holds, where it goes) and `grants` (an editor's role, of
  every team or of one), with the fields the UI's forms take. It is read
  strictly: a key it doesn't know, a value of another kind, a key given
  twice, a file over 1 MiB, more than the ledger keeps, an empty file, or a
  key with nothing after it is refused with its line, and changes nothing. Each thing is then checked as
  its form is: one that can't be kept is skipped and kept as it was, and
  the rest is kept. The leader keeps the ledger holding what the file says,
  in one write, as each term it leads begins and whenever the file's
  content changes, read and hashed every 15 seconds, as Kubernetes swaps a
  ConfigMap's files through a link; no restart. A file is kept to only once
  the same file has been read twice in a row, 15 seconds or more apart: one
  read half written changes nothing, giving no one a role, and a new file
  is kept to 15 seconds or more after it is first read, a new leader's 15
  seconds after it has read the rules: usually within half a minute, but
  that is no bound. Settings says when a file waits, a ledger write that
  failed, and files read for minutes none of them twice in a row, which is
  logged too. Each thing is matched by
  its name, a team's in any letter case or by `renamed_from`, a quiet
  window's by its title; what the UI made of the same team under that name
  is taken over. Each change is noted in the activity log by `config`. What
  can't be used or kept is shown on the Settings, Runs and Manage pages,
  why to admins alone, as it can name who holds a role. What a file
  declares has limits of its own, apart from the UI's.
- **What is declared is read-only.** The UI marks it "declared" and draws
  its actions disabled, "Declared in config — change it in values" beside
  them; the ledger refuses every write to it, from the forms, the API or
  anywhere, 409, and a team it names isn't deleted or renamed.
- **What it no longer declares** is the UI's again, as it was, or, with
  `--config-prune` (`serve.configPrune`), deleted once the same file is
  read twice in a row; a team something still names is given back to the
  UI instead. A role is taken away only when a file that was read twice
  no longer lists it, pruned or not. Given back past what the UI keeps, a
  thing is deleted. What the file no longer declares goes before what it
  adds. A team that can't be kept keeps what names it as it was. Without
  `--config`, everything is the UI's again, roles included, none taken
  away, marked given back: until the UI changes one, the next file that
  doesn't declare it again takes it away, a role, or deletes it, with
  prune, and one it declares again of another team is moved, not made
  again. The Settings page lists them, and an admin keeps one, or all, as
  the UI's there; the mark doesn't expire.
- **`GET /config.yaml`**, an admin's: every team, quiet window, report and
  role the ledger holds, declared and made in the UI alike, as `--config`
  takes them, linked from the Settings page beside `settings.yaml`. Read
  back, it changes nothing but that what it holds is declared.
- **Orphans** (`/orphans`, under Manage): the names no rule in effect has
  that the ledger keeps a note, tasks, comments or proposals not finished
  with of, a hundred a page. An editor moves what one kept to a rule in
  effect, for a rename, or deletes it, each noted; its proposals are
  listed, to be dropped on their pages. At 4,000 rules and 3,000 names
  gone, the page reads the names one at a time.
- **`--proposals-expire DURATION`** (`serve.proposalsExpire`), off by
  default: a proposal that waited for approval longer since it began to
  wait (made, or backtested again), and that no one approved, is closed as `expired` by the leader's work,
  noted in its history and the activity log, and its rule takes another;
  its page says when it would.

### Changed

- **A team's name is unique in any letter case**, for new and renamed
  teams, from the UI, the API and `--config`. Teams a ledger already holds
  whose names differ only in case are kept and listed on the Teams page,
  to be renamed; each can be changed but for its name.
- **A quiet window's title is one line**, with no character that doesn't
  show, as a team's name and a role's holder's are.
- A team deleted in the UI notes each role it takes away with it. The UI
  keeps 1,000 teams at most.
- Lapilli doesn't propose again by itself a change that expired.

## [0.43.0] - 2026-11-16

A deployment upgraded to 0.41.0 had a proposal 0.40 made still waiting:
the restart rule's `for`, 1m → 1h, from 0.40's suggestion. Its page said
nothing of a `for` one event can't outlast, as that was worked out only
when a proposal was made, and its backtest still said 141 firings → 3,
better, of weeks with no incident written down, where fewer may be the
restarts hidden. Now what a proposal's evidence says is worked out when it
is read, whatever lapilli made it; a backtest with no record is counted,
not judged; and a proposal whose backtest was replayed or judged otherwise
than this lapilli does, or whose period's record changed since, waits to
be backtested again before anyone approves it, which Lapilli does itself
too. No schema change: what is kept is in a proposal's evidence, and the
ledger's changes. JSON, the API and MCP add fields; MCP's `stale` is a
string now.

### Added

- **A proposal read as it is now.** On its page, the proposals list, a
  rule's proposals tab, its pull request's description, the API and MCP,
  a proposal of a `for` says whether one event can outlast it
  (`for_outlasts`), worked out from the rule it was made against, which
  its hash names, with the `for` proposed, as carrying it out makes the
  rule: of the rule in effect, or of the version in effect when it was
  made; one whose rule neither has, and one finished with (applied,
  dropped, refused, failed), says what it kept. Each proposal's is worked
  out once while serve runs, by its evidence too; a version is parsed
  once for every proposal made against it, by one of the requests asking
  at once, its matched rules alone kept, and four older versions a read at
  most: cold, two hundred proposals each against a version of its own
  take 20 ms to list, not a second and 1.4 GB. Whether its backtest's
  period had an incident written down to judge its firings by
  (`no_record`) is kept with each backtest from now on, as the ledger has
  the period when it runs (`ledger.RecordKept`), and worked out from the
  ledger for one made before, unless it judged some firings then (caught,
  missed or caught later an incident), when it had a record.
  `GET /api/v1/proposals` and `/api/v1/proposals/{id}` add `stale`,
  `for_outlasts` and `no_record`; MCP's `rule` gives each proposal's
  backtest (`backtest`: its firings, and, with a record, what it caught,
  missed and caught later).
- **A backtest with no record is counted, not judged.** Its table says
  once that no incident was written down, or found by `--impact`, in the
  period or the two weeks before it: "the firings are counted, not judged.
  Fewer firings aren't better, nor more worse: fewer may be events that
  happened, hidden." 141 → 3 is −138, neither better nor worse, and what
  was caught or missed is "–". Its pull request's description says it as
  `--compare` does of such a period, with no "catching an incident", and
  MCP gives its firings alone, with `no_record`.
- **Stale proposals, backtested again.** A proposal waiting for approval
  whose backtest was replayed or judged otherwise than this lapilli does
  (`semantics` in its evidence, `verdict.Semantics`, a number bumped only
  when replaying or judging changes; once, for one kept before it was;
  the `version` that ran it is kept beside it, `dev` for none, to be
  shown; a newer lapilli's, in a rolling update or after a rollback, is
  held and said so, and left for a person), or whose period's record
  changed since (an incident written down in its weeks, or none keeping it
  any more), is stale: its page says why, the list has a badge and links
  to `/proposals?stale=1`, which lists the stale ones (with those
  whose rule changed since), and `stale` says why in the API and MCP. It
  isn't approved until it is backtested again: its page shows the button
  disabled, with why, the server refuses it, and so does the ledger an
  approval of a proposal as it no longer is, backtested again meanwhile;
  nor does `--approvals` lowered approve one. With `--changes`, Lapilli
  backtests again itself, as Lapilli, those an earlier lapilli backtested,
  on a run of its work alone (never when a write pokes it): three a run,
  twelve an hour at most on every replica together, each counted as it
  begins, kept or not, the one begun longest ago last, one at a time
  under the heavy turn, each within ten minutes (`--timeout` when
  shorter), never waiting for a comparison running. One that doesn't
  finish replaces nothing, is said in the proposal's history (tries in a
  row as one line), ends the run, and leaves that proposal, and every
  other, an hour, then two, four, up to a day, until one is kept. A
  record that changed, or a newer lapilli's, is a person's. An editor backtests one again from its page (`POST
  /proposals/{id}/backtest`, asked for as a proposal is: the role, from
  this site, and the tries an hour, read again where it is kept), and an
  agent with `--mcp-propose`, through `backtest_again` (destructive, not
  idempotent): the change replayed with this lapilli against the rule in
  effect, over the twelve weeks to now. The ledger keeps them bounded, on
  every replica: twenty an hour a person, twenty for the agents acting for
  them, and a proposal not again for ten minutes. The backtest it replaces
  is kept in the proposal's history, with what it missed, caught and
  caught later and who backtested it again, an agent's said so; every
  incident a backtest named as missed is kept (`earlier_lost`), said on
  its page, in its pull request's description and through MCP
  (`earlier_missed`), and not refused when it is checked again as it is
  carried out. The approvals given to the backtest replaced are withdrawn,
  and told as `proposal.backtested` to the webhook and Slack, naming who
  gave them, when any were or an agent asked; MCP's `rule` gives each
  proposal's `backtested_again`. Refused when the rule changed since:
  propose it again. Every incident ID its pull request's description
  names is a code span of its own, nothing in it read as Markdown (one
  with nothing in it, `(none)`). What a proposal shows when read never
  stops a change being carried out: for_outlasts falls back to what it
  kept, and stale ones that can't be read are approved by none as fewer
  approvals are needed, that round.

### Changed

- MCP's `stale` on a proposal is why it is stale, a string, where it was
  `true`: the rule changed since it was proposed, as before, or why it
  waits to be backtested again.
- The proposals list reads, in brief, the evidence of a proposal of a new
  `for` and of one waiting for approval, beside an undo's.

## [0.42.0] - 2026-11-16

First use: what a first-time reader got stuck on, on a real deployment
(rules saying whose each is in a `team` label, no team, no incident, one
admin, nowhere to send a notice). No ledger schema change; the API and MCP
answer as they did.

### Added

- **Teams the rules' labels suggest.** The Teams page lists each value of
  a `team`, `owner` or `squad` label (`--team-labels`, or the chart's
  `serve.teamLabels`, names others) on rules no team owns, with how many
  it would own; an admin makes them with one button ("Make these teams"),
  each as the team form would, its name the value and its labels the
  label. Only a plain lowercase name is made (`pаyments`, its а Cyrillic,
  or `Web` beside `web`, would read as another team's); one whose label
  is on rules another team owns isn't, as it would take them; one whose
  name a team has, in any case, isn't, and the page says what to do as
  that team owns. The result names the teams made, by their ids, those
  alone the reader made or changed in the last ten minutes. With no team
  at all, the rules list says why its Team column is empty, and links to
  them.
- **Why a rule is a team's.** A rule's page says how its team was found:
  its note, its name matching a team's pattern, its labels having a
  team's; or that none matches.
- **Who may approve.** The Roles page says who may approve a change in
  effect (those `--approvers` names, or the admins, named; or no one, and
  why). A proposal waiting for approval says, to an admin, who may approve
  it, but the one it was made by or for, and, when no one is left or fewer
  than it needs, why and what to change; anyone else reads only that an
  approver approves it, or that no one can yet and an admin sees why on
  the Roles page.
- **Where notices go.** A Notifications card on the Manage page, and a
  section of the Settings page: where what happens is told, how many
  places reports and teams' digests can go, the weekly digest's time as a
  person reads it ("Mondays 09:00"), the last week posted to Slack and a
  later week's that failed, as the ledger has them, when the outbox last
  sent, and how many of the last day's notices failed and wait to be tried
  again and how many were given up on, each of a place's latest 1,000,
  past which it says more wait. The
  weekly digest, or a report, on with nowhere to go is said so in words,
  with the way out. Counts, never a URL. These two pages are worked out
  for each request, as a notice sent empties no page kept.
- `--team-labels LIST`.

### Changed

- **The firings open on the last day with firings**, said so, when no day
  is asked; with `day=`, as before.
- **A time a page shows is rounded**: "The last run took 3.4s.", not
  3.412345678s; a rule's `for` as before.
- **Korean says 정탐 and 오탐**, as DevOps does, not stones (돌);
  `TestKoreanSaysNoStones` keeps it so, the noun read as Korean has it.

### Fixed

- **A rule's team is found by its group's labels too**, its own over its
  group's, as its alerts carry them, for every source. A file's rule read
  its own labels alone, so a rule whose group says `team: payments` had
  the chip on the rules list (since 0.40.0) and `label:` picked it, while
  its page said no team's labels matched it and its team's list left it
  out; the ledger, judging a team's quiet windows, already took it for the
  team's.
- **The weekly digest could hold the runs up for twice `--timeout`.** In
  0.41.0 its count of the firings by team had a `--timeout` of its own,
  after the replays had used theirs. It has a minute at most now, or
  `--timeout` when that is shorter, so the digest holds the runs up for
  `--timeout` and a minute at most.

## [0.41.0] - 2026-11-15

A deployment with no incident source had every rule that fired ten times
or more called noisy, at 0% precision, and its noisiest suggested a longer
`for`. On one, a restart rule (`increase(restarts[30m]) > 0.5`, `for:
1m`) was told to wait an hour: every one of its firings was a real restart,
of services that failed to start for weeks, and an hour is longer than one
restart can hold the condition, so the suggestion would have hidden them
all. Now a firing with no record at its time is counted, not judged; a rule
that fires that often with none says what its firings were instead; a `for`
one event can't outlast is said to be one, and never suggested; and a
rule's firings are counted by the team whose labels they have. **Ledger
schema 018**, after 0.31's 017: an index of when incidents ended, so that a
page reads the incidents that keep its weeks' record and no others (a
lapilli before 0.41 doesn't open a ledger this one has migrated). What the
weekly digest works out is kept with its other results, from when
`--impact` was read in `meta`. JSON, the API and MCP add fields; none is
renamed.

### Added

- **No record, no verdict.** A firing is judged only when the record was
  kept at its time: an incident firings are judged against (written down,
  or an impact window that counts) was on at some time in the two weeks
  before it, one with no end taken to last an hour, as long as it is
  caught for, or it caught one; or an impact signal was read over its
  time. `--impact` is read from two weeks before the period, and keeps the
  record of all of it whatever it finds, as it says whether users were
  hurt; the firings are judged against the windows it finds in the period,
  as they were. In lapilli serve, from when its runs read it, with no run
  since that didn't (`impact_since` in `meta`: a run without `--impact`
  ends it; after an upgrade, it starts with the first run that reads it,
  as an earlier lapilli kept no word of when its runs did). Any other
  firing caught nothing written down, which can't be told
  from one nobody wrote down: it is counted, in the firings and the black
  stones, and is in no precision and makes no rule noisy. A rule none of
  whose firings is judged has a precision of `–`, and no longer `for` is
  suggested for it. `lapilli verdict` judges each firing so; lapilli serve
  judges by week, a week's firings by whether its record is kept, and,
  with a team picked, in its digest and its reports, by the incidents the
  team owns (a team with no labels, which owns one only when its rules
  caught it, by everyone's); over several weeks, or part of one, each week
  by its own, whole, so the rules list and the overview's weeks agree, and
  what it replays (a change's backtest, the try page, the digest's
  suggestions and near misses) by the same weeks, so that it compares like
  with like. Every precision serve shows is of the firings judged: the
  rules list's sum, the span before a rule's period, the outcomes' then
  and now, during deploys, a report's and MCP's. The text report, the digest, a report's
  noise line, the overview (where the noise is leaves such weeks out), the
  rules list and a rule's page say so once, with how to keep a record, and
  what came out of hours is said to have had no incident to judge it by.
  `--json`, the API, the summary and MCP have `no_record` on the report,
  the week and each rule none of whose firings is judged, `unjudged` counts
  the rest, and a precision of null. A comparison given incidents, none of
  which keeps its record, counts its firings and doesn't judge them, and
  its summary says `no_record` (README, "Pull request backtests in CI");
  `--check-content` says which rules are noisy can't be told. The rules
  list's Performance has a value for a rule that fired as often as a noisy
  one with no record (`? No record`, `perf=unrecorded`, `is:unrecorded`).
  The pages know it from the incidents read once with the weekly tallies,
  never from the firings.
- **What a rule's firings were, with no record.** For each rule replayed
  that fired as often as a noisy one with no record, the ten that fired
  most. Of a condition that counts events, each firing is one, a count that
  went up (a Grafana rule's on no data is none: with one among them, that
  isn't said, nor, of another condition, that it went past its bound): "Fired 141 times with no incident written down to judge them by.
  All 141 are events its condition counts (a count that went up): these
  happened. … A longer "for" would hide them: write down whether they
  mattered, or fix what fires them." Of another with one bound that
  decides it, how many had the value its condition compares go past it by
  more than a fifth of it, for its `for` (the rule replayed once with the
  bound that much further), not a flicker at its edge; of a range, `and`,
  `or` or `unless`, that is left unsaid, not guessed. How many alerts
  fired, and the five that fired most by the labels that tell them apart;
  in lapilli serve, the team that owns the rule, the team whose labels each
  alert has, and those during deploys. `lapilli verdict` works it out when
  it replays such firings (`"unrecorded"` in `--json`); the weekly digest
  for its week, everyone's and each team's by its own record, within its
  `--timeout`, its reads bounded (5,000 firings a rule, the 1,000 alerts
  that fired most, counted by the ledger: past 5,000, "Too many firings to
  tell how many happened", `too_many`), kept as an `unrecorded` result:
  the digest and `digest.ready`, a team's digest (another team's rule with
  the alerts on what the team owns alone), a rule's page in place of a
  suggestion (with its firings to mark the real ones, and a task), the
  overview first among what to look at, and MCP's `rules` and `rule`
  (its text without the alerts' labels, which are in `top`, as data) read
  it from there. A week written down for since shows nothing.
- **A `for` one event can't outlast.** A rule whose condition counts events
  (`increase`, `changes`, `resets` or `rate` of a counter's range or a
  subquery of one, summed or maxed by some labels or not, times or over
  numbers or not, compared with a number no event passes and one event
  does) and whose `for` is as long as the range or longer fires only on
  events that keep coming for all of its `for`: "Its for (1h) is as long as
  the 30m its condition counts over, or longer: one event alone can never
  fire it; only events that keep coming for 1h will." `delta` is a
  gauge's, `idelta` and `irate` hold for about a scrape, and a counter of
  seconds or bytes, or a histogram's `_sum`, counts an amount: none of
  these counts events. A Grafana rule counts events when its condition is
  a threshold of one number (`gt`, `gte`), or a math expression that is
  one comparison of a ref with a number, of its one PromQL instant query's
  value, as it is or through reduces that leave it so (`last`, `max`,
  `min` or `mean` of its one value): a query of `increase(...[30m])`, a
  reduce (`last`) and a threshold (`> 0.5`) is the restart rule. Not a
  range query, whose points one event can hold for as long again as its
  range, a classic condition, other math, a reduce that changes the value
  or puts a number in place of NaN, two queries, or a rule that fires on
  no data (`noDataState` `Alerting` or `KeepLast`). It is a fact of the content check (`for_outlasts`),
  on a rule's page and through MCP, and `lapilli verdict` says it of each
  rule it replays (`for_outlasts` in `--json`). A proposal of such a `for`
  is made all the same, as a team may want a rule for a crash loop, and
  says what it means on its page, in its pull request's description and
  through MCP; a rule's proposals tab says how long its condition counts
  over. An expression over 4 KiB, or nested 8 brackets deep, isn't read
  for it.
- **Firings on what a team owns.** The weekly digest counts each rule's
  firings by the team whose labels they had (the teams' labels, the first
  by name), from one grouped read of the week's firings by alert (the
  20,000 that fired most), and keeps the rules most of whose firings, more
  than half, came from what another team owns than the one their alerts go
  to (or no team's): kept as a `fired_on` result, read under a `--timeout`
  of its own, apart from the replays'; a rule's `unread` says how many of its firings were past the
  alerts read, and the digest logs how many in all. A rule's page says "Its
  alerts go to payments; most came from what web owns (14 of 14)", web's
  rules list lists them, web's digest names them, and MCP's `rules` and
  `rule` give `fired_on`.

### Changed

- **No longer `for` past what one event holds is suggested.** A noisy rule
  that counts events is replayed with longer `for`s shorter than its range
  only. Before, a rule that had caught nothing kept the first `for` that
  halved its firings, which for such a rule was the one at which one event
  could no longer fire it.
- **A stored suggestion follows the record.** The last digest's longer
  `for`s are shown, proposed and sent only while the week they were worked
  out for still has its record: not after an upgrade to a week that had
  none, nor once an incident is taken away.
- The overview names a rule once among what to look at: what its firings
  were with no record, when the last digest said it, in place of its
  suggestion or its noisiness.
- A rule with no record the span before isn't newly noisy in a report.
- Where the noise is names the rules with the most black stones judged.
- A result the digest kept that doesn't decode (suggestions, near misses,
  a change's backtest) is logged and taken as none, on every page and in
  the digest, rather than failing them.

## [0.40.1] - 2026-11-15

The two things docs/design/ui.md had left, and a disabled button that
looked as if it could be pressed. No ledger schema change; the API and MCP
answer as they did.

### Fixed

- **A quiet button, disabled, looks disabled.** The stylesheet's quiet
  button's own colours and border came after a disabled button's, with as
  much weight, so the rules list's bar ("Set the team", "Set the
  disposition", "Propose"), the overview's "Make it a task", a firing's
  "This was real" and a rule's header drew one a reader may not press as
  one they could (since 0.39.0). It is drawn as every disabled button is,
  dashed and dim.
- **A rule's header draws its disabled actions as every page does** (the
  `off` template): those disabled for one reason share it, said after the
  last of them. `TestDisabledOneWay` no longer leaves it out.
- **The overview's table of the weeks fits a laptop's screen.** Its first
  column kept the week and its marks' tags on one line (598 px in Spanish),
  and Spanish's headers alone were wider than the box; at 1280 to 1920 CSS
  px it was scrolled sideways in English, Spanish and Japanese, and at 1280
  in Korean. The date is kept whole and the tags go under it, the headers
  wrap between their words, and the cells are as close as the rules
  list's. `test/a11y/axe.mjs` checks it with the rules list's table.

## [0.40.0] - 2026-11-15

What docs/design/ui.md left for later phases: a proposal for the rules
picked on the rules list, and a rule's labels as chips on its row; the
rules table made to fit a laptop's screen; and every disabled action drawn
one way. Still no script, against WCAG 2.2 AA, in four languages. No
ledger schema change; the API and MCP answer as they did.

### Added

- **A proposal for the rules picked.** The rules list's bar proposes, for
  each rule picked, what its own page would: a new `for`, the `for`
  suggested for it, or retiring it (turned off where it can be, deleted
  where it can't). Each is the proposal the rule's page's form makes, made
  by the reader with every check it has: an editor's, read again,
  backtested under the heavy turn, one unfinished proposal a rule, twenty
  an hour a person; and each waits for a person's approval on its own
  page. At most twenty rules a request. The list says once how many were
  proposed, and why the others weren't (a proposal waiting already, that
  `for` already, no suggestion, not replayed, not in effect, the hour's
  twenty made), and links to the proposals made: those the reader made,
  read again, whatever the address says. A request starts no rule after
  twenty seconds, and says which weren't tried; a rule started before then
  takes what its backtest takes, within `--timeout` (50 minutes unless
  set), so behind a proxy whose timeout is shorter, raise it for
  `POST /rules/bulk` and `POST /proposals`, or lower `--timeout`.
- **A rule's labels as chips on the rules list**: the labels `label:`
  picks it by, but its severity, five a row and the rest behind "+N", each
  a link to the list with that label too.
- **The proposals list takes `id=`**, the proposals named alone, each a
  chip: the link a bulk proposal's result gives.

### Changed

- **The rules table fits a laptop's screen.** On a real deployment (62
  rules, Korean, long names and label values, 1440x900) its last columns
  were reachable only by scrolling it sideways. Names and label values
  wrap anywhere, a team's name between its words, a number's notes under
  it, a header between its words; the facets are beside the table from
  1600 CSS px (1200 before), above it narrower, side by side as they fit.
  `test/a11y/axe.mjs` checks the table against its box at 1280, 1440, 1600
  and 1920 px, with rules named as long as a real set's. The README's
  screenshot of the list is taller, to show the table under the filters.
- The overview's "Make it a task", the firings' "These were real" and
  "This was real", and the rules list's bar draw what a reader may not do
  as every other page does (phase 4's `off`), each reason said once.

### Fixed

- A proposal past the twenty an hour a person may make answered 500, and
  was logged as a server's error, when it was refused before the rule was
  read again, and 400 when two at once found the ledger's write past them;
  it answers 429, saying so, either way.
- A team's long name in a select pushed the bar, and the rules list's bulk
  bar, past the width of a 320 CSS px screen.
- The sidebar's languages broke 한국어 between its syllables; each name is
  kept whole.

### Security

- **What a refused proposal costs is bounded.** A refusal after the rules
  are read again, from where they are kept (Grafana, a ruler, a cloud's
  alarms, the files), isn't a proposal the hour's twenty count: a reader
  could ask for them without end, each a live read. A reader now asks for
  60 proposals an hour at most that read the rules again, made or refused,
  counted only when the rules are read again, and the agents acting for
  them 60 of their own, so that an agent's refusals can't use up a
  person's (kept in memory, a replica's own); past them, 429, before
  waiting for the heavy turn. Hardening of released behaviour.
- **MCP's `retire` reads the rule once, after the reader's checks.** It
  read every rule in effect, parsed whole, to decide between turning a
  rule off and deleting it, before checking that the person may propose at
  all; `propose` now decides, from its own read, after them. Hardening of
  released behaviour.

## [0.39.2] - 2026-11-15

The chart's workloads stay in sync under a GitOps tool that diffs what it
applied. No change to `lapilli` itself.

### Fixed

- **Argo CD showed the chart's Deployment as OutOfSync for as long as it
  ran**, with server-side apply and diff, though nothing had changed. The
  API server writes defaults inside fields that are applied whole (atomic):
  a `resourceFieldRef`'s `divisor` (the `GOMEMLIMIT` from the memory limit)
  and, in the projected service account token volume, a `fieldRef`'s
  `apiVersion`. The applied object didn't have them, so every diff saw them
  as a change. The chart now says both, with the values the API server
  would give (`divisor: "1"`, `apiVersion: v1`): the pods' values are the
  same.

## [0.39.1] - 2026-11-15

The chart's `lapilli serve` takes several rule ConfigMaps. No change to
`lapilli` itself.

### Fixed

- **The chart refused `rules.configMaps` of more than one in serve mode**
  ("serve reads one; put the rules in one ConfigMap"), though rules kept
  in several ConfigMaps (a Grafana's alert rules split by area, a shared
  set beside a team's) are common, and copying them into one is a second
  place to keep them. `lapilli serve` reads one directory: the chart now
  mounts every ConfigMap given as one projected volume, their keys side by
  side, and passes one `--rules`. Their keys must not share a name. The
  CronJob keeps a volume and a `--rules` for each, as before.

## [0.39.0] - 2026-11-15

Many at once, and ways in: the fifth phase of docs/design/ui.md, the last of
its plan. The rules list and the firings write to many rules or firings at
once, as each would one by one; a firing's labels are filters, and its day
is drawn by the hour; the overview's tiles and charts lead to what makes
their numbers; an incident's page draws when the rules fired around it.
Pages are worked out a few at a time, and two "This was real" at once
write one incident. Still no script, against WCAG 2.2 AA, in four
languages. No ledger schema change; the API and MCP answer as they did.

### Added

- **Many at once** (docs/design/ui.md). The rules list's table is one
  form: a checkbox a rule, and a bar to make a task for each, set their
  team, or set what is to become of them. The firings that caught nothing
  can be picked and written down as real together. Each item is the write
  its own form makes, in turn, with every check it has: a team's editor's
  rules of another team are counted, not changed, and the changes log has
  a line for each; a rule with a task to tune it still open has one
  already; an error that isn't the item's stops it there, and says how
  many weren't tried. At most
  100 a request, from this site; Enter in the form writes nothing. The
  list it came from is gone back to, its filters, order, team and period
  kept, and says once what was done ("3 done; 2 not your team's").
- **A firing's labels as chips**, each a link to the same day's firings
  with that label too (`/firings?label=service=api`, all of them, five at
  most); a label picked is a chip that takes it away.
- **A day's firings by the hour** over the firings' table: 24 cells from
  where the ledger's day starts, caught something hollow, nothing solid,
  excused dashed; the same as a table, which says an hour the clocks
  skipped or went back over.
- **The overview's tiles open what makes their numbers**: each links to
  the rules or the incidents over last week, the tiles' week, with the
  same team, and what the list comes to is the tile's number. The rules
  and the incidents take `week=DATE`, a whole week of the twelve kept;
  the incidents, `state=caught|noisy|missed`, each a link with its count;
  the rules list ends with what the rules shown come to, all told.
- **Marks on the overview's weekly charts**: the weeks with firings during
  deploys and the weeks a quiet window was over (up to now; with a team
  picked, one that applies to the team's rules), besides the rules'
  changes, each with a link to hide it or show it (`marks=`), in the
  legend and in the table of the weeks.
- **An incident's timeline** on its page: its span from `--lead` before it
  to `--lead` after, and a row for each rule that caught it, nearly caught
  it or was on around it, its firings drawn by their stones; twenty rules
  at most, fifty firings each; the same as a table.

### Changed

- The overview's "Make it a task", and the firings' "This was real", are
  shown disabled, with why, to a reader who may not use them, as each
  page's main action is: they were a form the write then refused, or
  weren't there.
- The firings' days before and after keep "only ● nothing", as the form
  does.
- Pages not kept already, a quiet window's preview among them, are worked
  out eight at a time; another waits its turn up to ten seconds, then is
  answered 503 with `Retry-After`. The
  turn is given back once the page is rendered, before it goes to the
  network, and while the page waits for the heavy turn. A page too big for
  the page cache isn't held twice, in the template's buffer and the
  cache's, and has 30 seconds to go to the network: forty-eight rules
  lists at once, at two thousand rules, peak at 550 MB of heap rather than
  1.4 GB. The API, MCP and the other ports don't wait, and an API's
  answer too big to keep has the server's two minutes, not 30 seconds.
- With a team picked, the overview judges the team's incidents against
  everyone's rules, as the incidents page does: one caught only by
  another team's noisy rule is caught only by a noisy rule, and isn't in
  "Time to catch".

### Fixed

- Two "This was real" pressed at once for one firing, by one person or
  two, wrote an incident each, and told of each; the bulk "These were
  real" beside them, more. A firing is written down in one write now, read
  again in it: once.
- "This was real" on a firing longer than an incident may be (31 days)
  answered 500; it answers 400, saying so.

## [0.38.0] - 2026-11-15

The way to change things: the fourth phase of docs/design/ui.md. Still no
script, against WCAG 2.2 AA, in four languages. No ledger schema change;
the API and MCP answer as they did. And a security fix, to what an agent
proposes with a sign-in of lapilli's own or in front, found by a review of
this phase.

### Added

- **A proposal's page, redone**, in Argo CD's order for a change: its id
  and what it changes in words ("Errors's for, 2m → 15m"), its state as a
  sign and a word (awaiting approval, approved, being applied, applied,
  undone, closed), who made it (an agent marked so), its way and when;
  the change as a diff; the backtest as a table, now beside as proposed
  (fired, caught something, incidents one misses that the other catches,
  incidents caught later), with a warning above it when the change misses
  an incident the rule catches now; the approval, n of m; and what
  happened to it, in order, each step with who and when, the pull
  request's link, a change waiting while its rule fires, and an undo asked
  for: the latest hundred steps, in the order the ledger kept them.
- **A diff with options**, `?diff=unified|split` and `?context=3|all`, as
  links, on a proposal's page and on a rule's versions tab: side by side is
  a table headed with its two sides; around each change, the lines the
  same further off are one row saying how many.
- **Facets with counts on the proposals list** (state, kind, way, made by
  a person, an agent or Lapilli), with chips and the team in the bar, as
  the rules list has them; the open ones unless asked, those waiting for
  approval first. The database picks the list and counts the facets, so
  every proposal counts, and newer ones can't push an older one waiting
  for approval off the list; a state there is no such of shows none, as
  it did.
- **A quiet window's preview**: a Preview button on the form to add or
  change one, the same POST, which writes nothing and is checked as saving
  is, shows the form as typed with the rules it applies to, the firings
  that caught nothing over the last four weeks it would have excused, and
  when it falls in the next four weeks.
- **A Manage page**, `/manage`: a card for Teams, Roles (admins only),
  Settings, Runs and Reports, each with what it is for. The sidebar's
  Manage links to it, and its pages' breadcrumbs start with it.

### Changed

- **Approving a change takes a box ticked**, "I have read the backtest",
  `required` on the page and checked by the server: an approval without it
  is refused, 400, and nothing is kept. So does asking for an undo, which
  is the undo's approval, one approver's, as it has been since 0.22.0 and
  as the help of `--approvals`, the README and changes.md now say plainly.
  `test/e2e/changes.sh` sends it. On a Grafana rule, the page says undoing
  puts back the whole rule as it was, annotations edited in Grafana since
  too.
- **Undoing an applied change is asked for as "Ask to undo it"**, and the
  page says what it has always been: a proposal of its own, approved by
  the approver who asks.
- **An undo keeps of the change it undoes only the edit.** It kept the
  change's evidence whole, so its page showed the words of whoever made
  the change, an agent's among them, as the asker's, and the change's
  backtest the wrong way round, with a warning of incidents missed that
  undoing it would catch. Its page now says it puts back the rule as it
  was before the change, links it and says whether an agent proposed it,
  and shows the change's backtest read the other way, "With #n" beside
  "After undoing it"; an undo made before is shown so too.
- **What a reader may not do is shown, disabled, with why**, in place of
  the form that was hidden: on a proposal's page (approve, ask to undo,
  drop or close, make it a task), the proposals' and the settings' switch
  that pauses changes, a quiet window's, a task's and a team's change, the
  settings' save, a report's change, an incident's change and whether it
  was real, and a rule's forms (what to do first, its note, a fix to what
  it says, proposing a change, a task about it).
- **The tasks' status is a facet with counts** (open, done, dropped), the
  team kept, in place of the status links; `?status=done`, `dropped` and
  `all` mean what they meant, and any other, `doing` among them, the open
  ones, as it did.
- The proposals are shown for the team picked in the bar, and a team's
  editor sees their team's unless they pick all teams.
- A quiet window's form has Preview as its first button: Enter in a field
  previews, and writes nothing.

### Fixed (security)

- **With a sign-in, an agent's proposal was no one's.** Since 0.30.0, with
  `--oidc-issuer` or `--proxy-jwt`, a change an agent proposed through
  MCP's `propose` (behind `--mcp-propose`, since 0.22.0) was kept as made
  by "anonymous": who the agent acted for was worked out from the
  request's headers alone, and these sign-ins keep who in the request
  itself, not in a header. So the person whose token the agent held could
  approve their own agent's proposal, alone with `--approvals 1`, which
  changes.md forbids; and every agent shared one twenty proposals an hour,
  which one of them could use up for all. Who an agent acts for is now
  worked out from the request to `/mcp` itself, for every way of signing
  in (a session, a bearer token, a sign-in in front, `--user-header`);
  with a sign-in, a proposal for no one known is refused; and each
  person's agents count against that person's own twenty an hour.
  **After upgrading**, look at the agents' proposals still open
  (`/proposals?by=agent`): one made before this release, with a sign-in,
  is kept as "anonymous"'s, and whoever its agent acted for may still
  approve it; drop any you aren't sure of, and let the agent propose it
  again.
  `--user-header` alone was not affected.

## [0.37.0] - 2026-11-14

A rule's page, redone (the third phase of docs/design/ui.md): a header that
says what to check, five numbers beside the span before, the last 84 days
in a strip, and its sections in five tabs. The rules list says what its
badges mean, and the README's pictures come from a demo ledger. Still no
script, against WCAG 2.2 AA, in four languages. No ledger schema change;
the API and MCP answer as they did.

### Added

- **A rule's page, redone** (docs/design/ui.md). Its header has its
  badges, its team, severity, `for`, group and runbook (its note's, or
  else the rule's, said so, with its host), and three actions: propose a
  change, add a task, edit the note, each shown disabled, with why, to a
  reader who may not; a team's editor is told whose editors may change
  another team's rule. "⚠ n to check" lists what to check about it, each
  with a link to where it is explained: problems of fact in what it says,
  its replay differing from what really fired, why it isn't replayed, a
  record copied in, an alarm with no history, a proposal made against a
  version no longer in effect.
- **A panel of five numbers** on a rule's page: precision, firings,
  incidents caught (and nearly caught), human cost (notifications sent,
  and those while people slept and off hours) and the backtest, each with
  a definition to open, over the page's period, each beside the span
  before it, as long, when what the number comes from holds all of it
  (the replay's runs; a source's copies, still coming; an alarm's
  history; the notifications kept), better or worse said in words as
  well as colour.
- **A rule's last 84 days**, a cell a day (caught something, caught
  nothing only, excused only, no firing), with marks for its versions,
  the days it fired during a deploy of what it is about, and the days a
  quiet window excused it or covered it, drawn on the server, its numbers
  in a table under it.
- **Each version of a rule beside the one before**, line by line, a
  label a line, on its versions tab, for the latest twenty.
- **What the badges mean**, on the rules list, near the filters: each
  badge, named as its facet names it, with a line on what it means.
- The README's screenshots are of a demo ledger with names anyone might
  use, served by a test (`TestDemoServe`) and taken by
  `test/screenshots/shots.sh`, with Docker or, with `--local`, without.

### Changed

- **A rule's page is in tabs**, `?tab=`: summary (what to do first,
  about this rule, incidents it caught, what it says), firings (latest,
  refired, notifications, while people slept, during deploys, the replay
  check), versions, proposals (proposals, the forms to propose a change,
  changes reviewed) and activity (tasks, comments), named apart from the
  sidebar's pages. A `tab=` it doesn't have is the summary. An old link to
  `/rules/NAME#says` lands on what it says, as before; one to
  `#proposals` or `#refired` lands on a line on the summary that links to
  the tab, `?tab=proposals#proposals` or `?tab=firings#refired`, where
  the links the pages make now go. A notice of a comment on a rule links
  to `?tab=activity#comments`.
- **A rule's page takes `weeks=`** (1, 4 or 12; twelve when not given, as
  before), picked in its bar: its numbers, re-fires, notifications, deploys
  and replay check are over that period, and its sentences no longer say
  "over twelve weeks". A link to it from the rules list, the overview's
  items over four weeks and the search from those pages carry their weeks,
  so its numbers are over the weeks the reader saw. `/api/v1/rules` and
  MCP's rule tool are as they were.
- **The rules list's "⚠ n to check" counts what the rule's page lists to
  check**, not only problems of fact, and links to that list; the
  Progress facet's "Problems to check" still picks the rules with
  problems of fact.
- A notice that a rule's replay differs links to the rules whose replay
  does, `/rules?trust=differs`, not the overview.
- The forms to propose a change to a rule are open on its proposals tab,
  not in a "Propose a change" `<details>`.

### Fixed

- In a time zone whose clocks skip midnight (America/Santiago), a date in
  a URL or a form (`/firings?day=`, a quiet window's until, a task's due,
  the summary's from and to) was read as the day before it.
- `TestA11yServe`, on which everyone is an admin, served wherever it was
  told; it, and `TestDemoServe`, listen on loopback alone, and stop
  cleanly when told to.

## [0.36.0] - 2026-11-14

`lapilli serve` runs as several replicas on one Postgres ledger: one leads,
and runs what must run once; every replica serves the rest
(docs/design/replicas.md). No ledger schema change: the replicas' own
tables are Postgres's alone, made as a ledger opens where they are older
than its lapilli's (`replica_schema`), with no DDL otherwise, and none is
copied.
SQLite is as it was: one process, the same files. Read "Before you
upgrade". Reviewed three times, as an attacker would, for correctness and
concurrency, and the fixes once more; what each found is fixed in what is
described here, as is what the first run of three replicas on kind found
(the end-to-end test asked the wrong port).

### Added

- **Replicas on a Postgres ledger.** Every `serve` on a Postgres ledger is
  a replica, alone or with others, sharing the ledger's advisory lock.
  - **A leader, on a lease** the database keeps (`replica_lease`), by its
    clock, at a term one more each time it changes hands. The leader runs
    the runs and the backfill, judging again, the incidents and deploys
    files, the check of the replay, near misses, proposals, the digest and
    the reports, the outbox, changes carried out (Grafana, a ruler, a
    cloud's alarms), the rules watched and their versions, the rules'
    history, the on-call feeds, and pruning. Every replica serves the UI,
    the API, MCP, `/metrics`, sign-in, and the incidents' and
    notifications' ports, and writes what it is sent.
  - **Takeover**: a follower takes the lease within 17 s of the leader
    being killed or cut off (15.2 s measured, the leader's process
    `SIGKILL`ed), within 2 s of it stopping, which gives the lease up.
  - **Fencing**: the leader stops leading by its own clock 2 s before its
    lease ends in the database, its work cancelled at once; each of its
    jobs looks at the lease first; each write it makes checks the lease in
    the database as it begins and as it commits (a deferred trigger that
    reads the lease `FOR SHARE`), and its clock before; each send waits for
    `Hold`, and for a claim the database checks under the lease. A leader
    whose lease went elsewhere, a renewal a failover lost or a host
    suspended among them, sends and writes nothing as the leader.
  - **A leader whose work is stuck gives the lease up**: work that makes no
    progress for 15 min (no job begun, no request asked of a history, no
    rule replayed), or a write held past its deadline (6 min), stops the
    renewals, and the lease is given up at once, though the work hasn't
    returned: another leads 15 min 7 s after the leader's last progress at
    most. `lapilli_serve_jobs_round_timestamp_seconds` says when it last
    finished a round.
  - **Each write takes the writers' turn across the replicas**: a row
    lock, which a role that may only connect can't take, waited for 30 s
    at most (`lock_timeout`); a write that waited longer is said in
    `lapilli_serve_ledger_writes_waiting`, and every replica stays ready,
    the turn being one for all. What reads then writes (merged values, the
    limits on windows, roles and proposals, approvals, `TellOnce`) is as
    right as with one process. The server ends a session cut off in a
    transaction a minute after its last statement: 60 s idle in it, or 60
    s with its answer not taken in (`tcp_user_timeout`, Linux). A write of
    an older replica after a newer lapilli migrated the ledger writes
    nothing, and the replica stops.
  - **The caches told of the others' writes**: the generations pages are
    cached by, and which weeks' firings changed, kept in the ledger
    (`replica_gens`, `replica_weeks`) and read before each request, one
    read at a time, within 5 s, which no write waits for: a write on one
    replica shows on the next request to any other. Ids cached are dropped
    when another replica deleted or renamed some, and everything is read
    again after a failover that lost commits (a nonce logged with each
    generation). A writer that connects again to the same server, as
    after a write cancelled in the middle, keeps what it cached.
  - **Notices claimed one at a time** (`FOR UPDATE SKIP LOCKED`, under the
    lease), due and claimed by the database's clock: two leaders for a
    moment never send one twice, a claim taken late in a long round still
    lasts, and a replica whose clock is ahead delays none. The digest
    posted to Slack is claimed for its week too.
  - **Rate limits shared**: the tokens' and the notifications port's
    buckets are the ledger's, so several replicas let through what one
    did; the busy guards (one heavy request, one write a token) stay each
    replica's.
  - **Sign-in across replicas**: a sign-in's state is used once among
    them, kept once the provider signed in someone a role lets in (a
    sign-in that signs no one in, or a stranger's, writes nothing), swept
    at its time and bounded, past which each replica's own set checks it;
    the approval forms' key is the ledger's. Replicas compare their
    session keys' fingerprints: one with a newer key than the live
    replicas' joins, as a rollout of a new key brings it in, and
    `lapilli_serve_session_keys` says how many keys sign sessions; one
    with an older key than another live is refused, and without
    `--session-key-file` a second replica too. With replicas, a session is
    refused while who signed out can't be read, and a sign-out made then
    is kept, here at once and in the ledger once it can be.
  - **A follower shows what the leader is doing**, as the leader says it
    every 2 s; it reads the rules for what they say, and the on-call turns
    kept, as the leader does, without recording a version.
  - **`/readyz` on the metrics' port says which replica leads**, and
    whether its writes wait; on the UI's, ok alone. A replica that can't
    reach the database isn't ready. `/metrics` has `lapilli_serve_leader`,
    `lapilli_serve_replicas`, `lapilli_serve_ledger_writes_waiting` and
    `lapilli_serve_session_keys`.
- **The chart's `serve.replicas`**, more than one on Postgres alone:
  `RollingUpdate` one pod at a time (`maxSurge: 1`, `maxUnavailable: 0`),
  a PodDisruptionBudget (`serve.podDisruptionBudget.maxUnavailable`, 1,
  a percentage taken as Kubernetes rounds it, up), and a session key
  Secret of your own (`serve.signIn.sessionKeySecret.name`) required with
  `serve.signIn.oidc`. One replica stays `Recreate`.
- `test/e2e/replicas.sh`: three replicas on kind, the leader's process
  killed, another leading within the lease, every notice once, a rolling
  restart; and `test/e2e/postgres.sh` runs three replicas and kills one.

### Changed

- **A second `serve` on a Postgres ledger is a replica**, no longer
  refused. `lapilli ledger copy` takes the ledger alone, as serve did, and
  is refused while any replica runs; a replica is refused while a copy,
  or a serve of 0.35 or before, holds the ledger alone. Replicas don't
  count themselves in `schema_version.writers`.
- A Postgres ledger's migrations take the writers' turn, so replicas
  started at once migrate it once; a replica that finds the ledger
  migrated by a newer lapilli stops. A migration's DDL waits 2 s at most
  for a lock on a table (`pg_dump` holds one on each for the whole dump),
  and is tried again every 10 s, for 5 min.

### Before you upgrade

- **From 0.34 or 0.35 with `serve.ledger.postgres`, upgrade with
  `serve.replicas: 1`** (the default), then raise it: the older pod holds
  the ledger alone and refuses new pods until it stops, which `Recreate`
  waits for and `RollingUpdate` would not.
- **Each request to a Postgres ledger's serve asks the database once
  more**, for the others' writes, one replica or several.
- **Give the database to the ledger's role alone** (`REVOKE CONNECT ON
  DATABASE lapilli FROM PUBLIC`): a role that may only connect can still
  take or wait for the ledger's advisory lock, and keep replicas from
  starting or connecting again (docs/design/replicas.md).
- **Going back to 0.35 or 0.34**: its `lapilli ledger copy` refuses a
  ledger with the replicas' tables; drop them first, with serve stopped
  (the statement is in docs/design/replicas.md, "Failure cases").
- **Changing the session key with several replicas**: write the new key
  into the Secret, then restart the Deployment; until the rollout is done,
  a session works on the pods of its key alone (docs/design/signin.md).

## [0.35.0] - 2026-11-14

The web UI's frame, and the rules list, redone: a sidebar in four groups,
breadcrumbs, the team and the period carried on every link, a search on
every page, a page's main action in its header, and on the rules list
badges, filters, a search box and chips (docs/design/ui.md). Still no
script, in four languages, against WCAG 2.2 AA. The API, MCP and the
ledger are as they were. What two URLs show has changed, `weeks=1` on
the incidents and no `team=` for a reader of one team, and a page's
query has bounds: see Changed.

### Added

- **A sidebar** of the pages in four groups: See (overview, rules,
  incidents, firings), Fix (tasks, proposals, quiet windows), Report
  (reports, outcomes) and Manage (teams, roles, settings, runs), with the
  open tasks and the proposals to approve beside theirs, and Roles for
  admins. On a narrow screen, or zoomed in, it is a menu that names the
  page the reader is on. Every page has a breadcrumb: group, page, item.
- **The team and the period on every link.** Picked in a bar at the top
  of the pages that show them (`team=` on the overview, the rules, the
  incidents and the tasks; `weeks=` 1, 4 or 12 on the rules and the
  incidents, which now take one week too), and carried by every link
  between pages. With no `team=`, a reader who edits one team, and only
  one, sees that team's, and the bar says it is theirs; `team=` alone is
  all teams.
- **Search**, `/search?q=`, in every page's bar: rules by name, incidents
  by id or title, teams by name, fifty of each at most. One found is
  opened, but a rule or incident named `.` or `..`, which a redirect would
  make another page; a search with a rules filter is the rules page with
  it; nothing found says so.
- **The rules list's badges**, on three axes and what is under way:
  ● Noisy (with its weeks running), ○ OK, – Silent, ⧉ Spare; ≠ Differs,
  ◌ Not replayed, ⇣ Recorded only; keep, tune, retire; ✎ Proposed,
  ⚠ n to check. Each a sign and its name.
- **Filters on the rules list** by performance, severity, trust,
  disposition and progress (`perf=`, `severity=`, `trust=`, `status=`,
  `has=`), each value counted as picking it would show, and a search box
  that takes words of a rule's name and `team:`, `severity:`, `is:`,
  `status:`, `has:`, `label:KEY=VALUE` and `group:`. Those a facet has
  become its parameters, by a redirect; what the box doesn't understand is
  named and left out. `key:"a value"` takes a value with spaces. Each
  filter in effect is a chip that takes it away; an order keeps every
  filter; "Clear filters" keeps the team, the weeks and the order; the
  header says "14 of 63 rules". A severity is the rules' own spelling,
  whatever its case in the URL.
- **A page's main action in its header**: "Write one down", "Add a task",
  "Add a quiet window", "Add a team", "Add a report". To a reader who may
  not, the button shows disabled, with why: read-only, no one signed in,
  editors only, admins only, nowhere to send a report.
- Empty states of three kinds, alike on every page: nothing judged yet,
  nothing matching (with the ways out), nothing to do.

### Changed

- **`/incidents?weeks=1` shows the last week.** It showed four, as every
  `weeks=` but 12 did.
- **With no `team=` in the URL, a reader who edits one team, and only one,
  sees that team's** on the overview, the rules, the incidents and the
  tasks, where they saw every team's. `team=` shows every team's; the API
  and MCP take a team only when asked, as before.
- **A page asked with a query of more than 8192 bytes is refused, 414**,
  before anything is read for it. A search (`q=`) of more than 500
  characters is left out, and the page says so; of its words, the first
  20 are used and the rest named; a filter (`perf=`, `severity=`,
  `trust=`, `status=`, `has=`) takes 20 values. Several `q=` are one
  search.
- An error page (403, 404, 414, 500) shows no count of open tasks or
  proposals, and carries no team or period on its links.

### Fixed

- **An error page, and the page after signing out, showed how lapilli
  serve's runs were going, and its version, to anyone (low).** A stranger
  refused (403), a page not found, and the signed-out page, which anyone
  may open, had the status line: what is being done, a failed run's error
  as the API gave it (a host inside among it), whether changes are
  paused, and the version. They have none of it now.
- **Signing in from a page asked with a long query failed.** Where to go
  back to was kept in the sign-in's cookie whole, and past about 2.9 KB of
  it the browser dropped the cookie: the sign-in "didn't start here". Past
  2048 bytes the page is gone back to without its query.
- A task's change of status, and its deletion, come back with the team and
  the period, as adding one does.
- The rules list's Rule column has badges for the sentences it had under
  a rule's name; the note's status is a badge, and the Noisy column is
  gone. The suggestion stays, as one line.
- The team is picked in the bar alone: the overview, the incidents and the
  tasks no longer have a row of team links of their own.
- The rules that didn't fire in the weeks shown, listed below the rules
  list, include those that fired in the twelve weeks kept but not in the
  weeks shown, which weren't on the page at all.
- The rules list, and `/api/v1/rules`, work out each rule's weeks with a
  re-fire for that rule alone: over two thousand rules and twelve weeks,
  330 ms became 60 ms.

## [0.34.1] - 2026-11-14

A security release, after two reviews of 0.33.0 (whose code 0.34.0
holds) from an attacker's point of view: of what comes in to lapilli serve's own sign-in, and of what the
APIs lapilli reads (a history, a ruler, a Grafana, a cloud, an on-call
feed) can make it hold, wait for, or say; and a third review of the
fixes, whose findings are fixed in what is described here. Each fix comes
with a test that failed before it. No ledger schema change. Read "Before
you upgrade".

### Fixed (security)

- **A bearer token outlived the record of its person signing out.** An ID
  token sent as a bearer token was taken until its own `exp`, which the
  identity provider sets (Dex's are 24h), while who signed out is kept for
  `--session-max-age` (12h): a token copied before its person signed out
  was taken again once the record was pruned, at a restart or when anyone
  else signed out. A bearer token is now refused once it is older than
  `--session-max-age`, as a session's cookie is.
- **Strangers signing out could push an admin's sign-out out of the
  record.** `POST /auth/logout` needs a session cookie, not a role, and the
  identity provider gives one to whoever it signs in; the record holds
  10,000 people, and dropped the oldest. With a provider people sign up
  to, 10,000 sign-outs under names of one's own made a copy of an admin's
  cookie, refused since they signed out, good again. Only people a role
  lets in are kept now, and a full record fails closed: the one let go of
  raises a time for everyone, and every session and bearer token made
  then or before is ended.
- **Signing out wrote the ledger for anyone, each time.** With no role, no
  rate and no look at whether the session was still on, under
  `--read-only` too, each POST rewrote the record (440 KB, full), and
  every request read it back. A session signed out of already now writes
  nothing (its cookie is still taken away), nor a stranger's, and a
  request reads the record again only when the ledger counts a change to
  it. Under `--read-only` a sign-out of someone a role lets in is kept:
  signing out must work, and SECURITY.md now says so.
- **Each `GET /auth/login` left a cookie, and none was taken away.** A page
  elsewhere could send a browser there again and again; past 180 cookies a
  site, browsers drop the least recently used, the session's among them.
  A login now takes away the sign-ins under way the browser holds but the
  newest three.
- **A Grafana that never answered held lapilli serve's work loop
  (medium).** The weekly proposals read the rules from Grafana again with
  no deadline, through a client with no timeout: every hourly run, digest,
  change and report waited on it. A read of the rules gets a minute,
  whatever starts it, and the client to Grafana gives up after a minute,
  on its headers after 30 seconds.
- **Replayed alerts' labels were kept as an API answered them (medium).**
  The ledger kept the labels of recorded firings, notifications and
  incidents to 4 KB, but not those of the firings it replays: a hostile
  API's few KB on the wire, gzipped, were tens of MB in the ledger and its
  cache, more each run. They are kept as the others now: 4 KB in all, a
  name of 128 bytes and a value of 256, cut past that with a hash of what
  was cut, a value with what turns text around in it likewise, and a set
  still past 4 KB with its longest labels folded into one hash
  (`__lapilli_cut__`, which an API's own label of that name is folded
  into too, as is one named as another is kept), so alerts apart stay
  apart. lapilli serve's replay check takes what really fired as it would
  be kept. A rule's replay in a run keeps 20,000 alerts at most, told
  apart by their labels (by a hash seeded as the process starts, which an
  API can't make two sets share): past that the rule is left as it was,
  as one whose query failed, and says why. The cache of ids and labels
  holds 32 MB at most.
- **A Prometheus's configuration could take hundreds of MB (medium).**
  `/api/v1/status/config`, a read lapilli does without, could be 256 MiB
  once its gzip was taken off, and its YAML was parsed whole, some 90 times
  its size: 4 KB on the wire took 360 MB. An answer over 2 MiB is refused
  before it is parsed now, and the active targets over 32 MiB; either
  said as a warning in lapilli serve's log, and on lapilli verdict's
  stderr, as the range check is off then.
- **A ruler's or a Grafana's rules took some 280 to 350 times their size
  (medium).** A list was read whole into YAML nodes, written out again
  and parsed into rules: 4 MiB of small rules took over a GB. A list, or
  a Grafana's export, is still 64 MB at most, but read at a value every 4
  bytes at most, past its first 100,000, and 6 million in all (counted
  before it is parsed: real ones are a value every 5 to 15 bytes), and of
  50,000 rules, counted before they are written out again (a list that
  doesn't parse whole too) and as read; each group is written out on its
  own, of 100,000 values at most (a namespace with one larger is passed
  over, as one that can't be read is, and so is one written whole, in
  flow style, of more), and a document's nodes let go of as they are
  read. What a ruler's API, a Grafana's export or a cloud's API
  writes holds no YAML alias, and one that does isn't read: a ruler's
  namespaces were each a document, their aliases expanding on their own,
  and 99 KiB read as 230,400 rules in 306 MiB. Those 4 MiB of rules are
  refused in 133 MiB; 40,000 rules in 2,000 groups, 1.7 MiB, read in
  132 MiB (579 MiB before); a Grafana export of 25,000 classic rules,
  62 MB, in 1.4 GiB (1.8 GiB before); and what a hostile list or export
  can be made to take within the bounds is some 85 times its size, and
  1.9 GiB, at most.
- **A range answer was kept whole, whatever it held (medium).** Samples
  outside the range asked, more than its steps, and any number of series
  were kept, and the 256 MiB an answer may be counted only once its gzip
  was off: 24 KB on the wire held 360 MB. An answer is read as it comes
  now, and only what was asked kept: a sample outside the range dropped; a
  series of more samples than the range's steps, or taking more of the
  stream than they could need, unreadable; past 50,000 series or
  5,000,000 samples, too big, and asked again over a shorter range, as
  when the API says so; past 128 MiB of labels (50,000 series of 2.7 KB
  each; a series of tiny labels repeated gzips to nothing), too many or
  too wide series to read, over any range. The answers to one range, its
  days and the halves the API asks for, hold 100,000 series, 256 MiB of
  labels and `--max-samples` samples (10,000,000 by default) in all at
  most, and past one the rule isn't replayed, said with the bound and how
  to read it all the same: an API saying every range of more than a step
  is too large, and answering each step with series of its own, had a day
  held as 72 million series. Answers read so are some 1.5 times faster, in
  half the memory.
- **A mapping of many keys took time with the square of its keys
  (medium).** A rule document is decoded with go.yaml.in/yaml/v3, which
  checks a mapping's keys for duplicates pair by pair: a rule of 96,000
  label keys, a 1 MiB answer from a ruler or a Grafana (or a cloud's
  alarms, as JSON is YAML), held a CPU for 20 seconds, past any
  `--timeout`; 64 MB, for hours. Documents are decoded to their nodes,
  which takes time linear in them, and then to values as yaml.v3 would
  (fuzzed against it: the same, but near yaml.v3's bound on what aliases
  expand to, which is kept, counting the keys a merge brings in again); a
  mapping of more than 10,000 keys, or values nested deeper than 200, are
  refused, and the context is looked at between documents and within one.
  What aliases expand to is bounded as yaml.v3 bounds one document's for a
  whole input: a stream's documents, a directory's files, the rule files
  a ConfigMap or an Amazon namespace holds, together. A fault in reading
  a document is an error, not a crash. A ruler's unit, a cloud's alarm
  and the hash of either are decoded so too. Rule files read a little
  faster.
- **An on-call feed could hold a CPU, with no deadline (low).** Replaced
  occurrences (`RECURRENCE-ID`) and a repeat's days (`BYDAY`) were
  searched each step, and a `TZID`'s zone loaded for each date: feeds of
  2 to 8 MB took 13 to 20 seconds and more, past a deadline as lines were
  read without a look at it. They are looked up now, a zone loaded once a
  feed (1,024 zones a feed at most, more than IANA has), the lines and the
  repeats read with a look at the context, and each feed read and parsed
  within a minute.
- **Two errors quoted an unreadable answer as the server wrote it
  (low),** a result type that isn't a matrix or a sample that isn't a
  number, without taking out the token, as every other error of the API's
  does; `--markdown` puts that reason in a pull request's comment. Now
  taken out, before the text is cut, so a long token's start doesn't
  stay; and from an answer that isn't the API's, said from its first
  4 KiB, the start of a token cut there.
- **What a ruler, a cloud or Grafana answered with an error kept a token
  it echoed (low),** on every page's status line, in a change's reason and
  in Slack's `change.failed`. The token, signature or credential the
  request carried, and any bearer token, are taken out as from a history
  API's errors. IAM Credentials' refusal, and the source of a namespace a
  ruler's read passes over, are said without what doesn't show.
- **One slow data source behind a Grafana held every place (low, 0.33.0).**
  The data sources behind one Grafana share `--parallel` places, held as
  long as a request is: a slow one, as a broken or hostile Loki behind it
  can be, took them all, and a healthy one's queries waited for its
  answers. One holding places doesn't take the last while another holds
  none that asked within ten seconds (looked at again when that ends, not
  only when a place is given back), and a place given back goes to the
  one waiting that holds fewest.
- **A direction override in a rule's name turned text on the pages around
  (low).** A name, a group, labels and what a rule says are read with
  what doesn't show as written or turns the text around it (a control
  character, a direction mark, embedding, override or isolate) as U+FFFD,
  as a tracker's comment is taken, from every source, its group's labels
  too; replayed alerts' labels are kept so too. An expression isn't
  changed, as its query is its own, but is shown so on the pages, and the
  pages set names apart in `<bdi>`, those in a sentence too.
- **`--metrics` wrote an escape sequence of a name as it was (low).** A
  name an API gave reached stdout raw, an OSC 52 sequence setting the
  terminal's clipboard among them. `--metrics` and serve's `/metrics` say
  a control character, and a direction mark, embedding, override or
  isolate, as every other output does (`\u{1b}`); the joiners names in
  Persian, Indic scripts and emoji are written with are as they are, as
  `ALERTS` has them.
- `*@DOMAIN` and `--oidc-hosted-domain` compare domains by their ASCII
  letters alone: Unicode's folding takes the Kelvin sign for a `k` and a
  long s for an `s`.
- A group holding a character that doesn't show (a zero-width space, a
  direction mark) is dropped, from a token, a signed header or
  `--groups-header`, as docs/design/signin.md said: before, only a token's
  spacing dropped one.

### Changed

- `--max-samples N`, for `lapilli verdict` (`--compare` and
  `--check-replay` too) and `lapilli serve`, and the chart's
  `maxSamples`: the most samples the answers to one range, a rule's query
  over the period, may hold in all (default 10,000,000, some 600 MB at the
  peak, 64 bytes a sample).
- The weekly fuzzing in CI takes five targets more: range answers, rule
  documents' values against yaml.v3's, on-call feeds, a JWKS, and a load
  balancer's signed header.

### Before you upgrade

- **Bearer tokens older than `--session-max-age` (12h by default) are
  refused**, whatever their `exp`. An API client or an agent holding a
  token of a provider that gives longer ones gets another sooner.
- **Signing out.** Only people a role lets in are kept as signed out: a
  person with no role who is given one later is let in by a cookie of
  theirs from before they signed out, until it expires. A record full of
  10,000 people within `--session-max-age` signs everyone out who signed
  in before the oldest of them: with `--viewers '*'`, or a provider people
  sign up to and a broad `*@DOMAIN`, 10,001 accounts signing in and out
  within it, again and again, sign everyone out, which takes an account
  each, and is a way to keep people signing in again, not a way in. Under
  `--read-only`, signing out writes the ledger's record of it. Two
  processes sharing a ledger should both be 0.34.1: an earlier one's
  sign-outs aren't counted, and a 0.34.1 doesn't see them until it starts
  again.
- **New bounds on what is read.** They may refuse an answer, or cut what
  is kept of it:
  - a ruler's list, a Grafana's export: 64 MB (as before), 50,000 rules
    as written and as read, a group of 100,000 values, and no YAML alias
    (nor in a cloud's alarms);
  - a rule document, from anywhere (rule files, a ruler's unit, a cloud's
    alarms): a value every 4 bytes past its first 100,000, 6 million in
    all, a mapping of 10,000 keys, values nested 200 deep, and aliases
    expanding as far as yaml.v3 allows one document, for a whole input
    (a stream's documents, a directory's files) together;
  - a range answer: 50,000 series and 5,000,000 samples, then asked over
    a shorter range; 128 MiB of labels, then not read; samples outside
    the range asked are dropped; the answers to one range together:
    100,000 series, 256 MiB of labels and 10,000,000 samples;
  - a replayed alert's labels: a name of 128 bytes, a value of 256, 4 KB
    in all, cut with a hash past them, as is one with a direction mark.
    An alert whose labels are cut is another series in the ledger from
    the upgrade on, and its earlier firings are replaced as the runs
    reach them. A rule's replay in a run: 20,000 alerts;
  - Prometheus's configuration 2 MiB, its active targets 32 MiB: past
    them, read as when it isn't served, and said;
  - an on-call feed: 1,024 time zones.
- **A rule's query over the period reads 10,000,000 samples at most**
  (`--max-samples`): 1,000 series firing all through a week at a minute,
  250 through 28 days. Past it, the rule isn't replayed, as one whose
  query failed, and the reason says so; 0.34.0 read it, in some 64 bytes
  a sample at the peak. Give a larger `--max-samples` (the chart's
  `maxSamples`), and memory for it times `--parallel`; or a longer
  `--step`, or a shorter `--since`.
- **A rule whose name holds a control character or a direction mark** is
  read with U+FFFD in its place: a new rule in the ledger, and a change
  proposed to it isn't found where the rule is written.

## [0.34.0] - 2026-11-14

The ledger where a team keeps the rest of what matters: in a Postgres
database, with its backups, failover and SQL, beside SQLite, which stays
the default. Still one replica; what more would take is in the design
note. Reviewed three times, as an attacker would, for correctness and
concurrency, and the fixes once more; what each found is fixed in what is
described here, as are two things CI's Postgres runs found (a server that
stops answering, which nothing waited on for long enough, and a page's
round trips). No change to a SQLite ledger's schema or files.

### Added

- **The ledger in Postgres** ([docs/design/postgres.md](docs/design/postgres.md)):
  `lapilli serve --ledger-url-file FILE` keeps the ledger in the Postgres
  database (15 or later) a connection string names, read from a file and
  said nowhere, in no flag, log, error, page, `/metrics` or MCP answer.
  To another machine, a string that says no `sslmode` (pgx's `prefer`) is
  refused, saying to ask for `verify-full`; one that checks no certificate
  is warned of at start. The string is read by pgx alone, as `psql` reads it.
  The same tables and statements as SQLite's, every text compared byte by
  byte, every key of any length and every label kept, so every page and
  answer is what SQLite gives; the ledger's and serve's tests run on both,
  on Postgres 15 and 18 in CI. Still one replica: the process that writes
  holds an advisory lock on the ledger's schema, a second is refused,
  saying which session and role hold it, and one whose connection dropped
  takes it again, or stops, and sends nothing more, if another process
  took it in between. SQLite stays the default, its files as they were:
  upgrading changes nothing there.
- **`lapilli ledger copy`**: a ledger copied into an empty one, a SQLite
  ledger into Postgres or one back, in one transaction, ids kept and no
  deleted id given again, both read back table by table and checked
  before it commits, the ledger copied from read as of one moment. A
  ledger that isn't empty is refused.
- **The chart**: `serve.ledger.postgres.secret` names the Secret holding
  the connection string, mounted whole so it can hold the server's CA; no
  PersistentVolumeClaim is made, and with the egress NetworkPolicy,
  `serve.ledger.postgres.to` says where the database is.

## [0.33.0] - 2026-11-13

Reading history from an API with a request quota, as a managed Prometheus
has one. Five `lapilli verdict --compare` runs at once against one were
answered HTTP 429, and three rules ended "Could not replay": the retries
gave up within 30 seconds, nothing sent fewer requests when the API pushed
back, and a run of eight minutes said nothing until its end. Fitted to
0.32.0's pull request backtests: one line between a history that couldn't
be read and a query it refused, drawn alike for `--fail-unreplayed` and for
the comment. Reviewed three times, as an attacker would and for correctness
and concurrency; what each found is fixed in what is described here. No
ledger schema change; the exit statuses, and every field `--json` gave, are
as they were, `--fail-unreplayed` aside.

### Added

- **`--max-rate N`**, for `lapilli verdict` and `lapilli serve`: each API
  the history is read from (`--prometheus`, `--loki`, its ruler included,
  and a Grafana's data sources together) is asked N requests a second at
  most, evenly spaced; `0.5` for a quota of 30 a minute. N is 0.001 at
  least.
- **A progress line.** A `lapilli verdict` run past 15 seconds says on
  stderr, every 30 seconds, the rules replayed of all (in a comparison, the
  replays before and after), the requests to the history, those asked again
  after a wait and those answered HTTP 429. `--progress=false` keeps it
  quiet. A run the history answered HTTP 429 says so at its end either way.
  stdout is unchanged. The backtest recipes copy stderr to the job's log as
  it is written, so a long backtest no longer looks hung; none of the
  progress reaches the comment.
- **`--fail-unreplayed`**: `lapilli verdict` exits 1 when a rule's replay
  failed as the history couldn't be read: not reached, not let in (HTTP
  401, 403, 407, credentials that couldn't be had), not found (404, a
  redirect to sign in, a proxy's or a gateway's page), or turned away to the
  end (408, 429, 5xx). Not for a query the history refused as written, nor
  for a Grafana rule replayed whose query outside its condition failed,
  which the report notes as before. Without it the exit is as before: 0, or
  1 when `--timeout` cut the run short. The backtest recipes refuse it:
  `FAIL_ON_ERROR=true` fails the job on every rule that differs that
  couldn't be replayed, whatever why, as before.
- `unreplayed` in `--json`, of a report and of a comparison: each rule that
  couldn't be replayed, its side in a comparison (none for a rule alike in
  both, replayed once for both), why, and `failed`, true when its replay
  failed for the run (the history unread, the time run out) and another run
  may replay it; false for a rule no run replays (a CloudWatch metric alarm,
  a rule paused, a query refused as written). In a comparison, the rules of
  it with a side are those `summary.unreplayed` counts, each once.
- `--compare --markdown` says, under its first line of counts, how many of
  the rules that differ couldn't be replayed, as `summary.unreplayed` does,
  since what follows leaves them out; the list under the tables says why, as
  before.

### Changed

- **Two kinds of failed query**, which a rule's reason says.
  `query failed: the history couldn't be read: ...` is a failure of the
  run, as above: `--fail-unreplayed` counts it, the text report names it at
  its top, and `--markdown` says only that, its whole reason going to
  stderr. `query failed: ...` otherwise is the history's answer to the query
  as written, which every run gets: refused in the API's own words (HTTP
  400 or 422), too large however short the range, not done (501), or an
  answer that can't be read. Both begin `query failed: ` as before, so what
  reads reasons by that, as the recipes' filter of the job's log does,
  reads them alike. JSON of another shape than the API's, such as a
  gateway's `{"message": ...}`, is said as `not a Prometheus API response`,
  as a page is.
- **Asked again after a wait the API says.** An answer of HTTP 429, or 503,
  with `Retry-After`, in seconds or as an HTTP date, is asked again that
  long after, a second at least and two minutes at most, plus up to a
  quarter so that requests told alike don't come back together, as often as
  it says so. A 429 without it is asked again after 4, 8, 16, 32, 60 and 60
  seconds, each less up to half, 90 seconds to 3 minutes in all, past the
  window of a quota by the minute; before, 2, 4, 8 and 16 seconds, 30 in
  all. A 5xx, or a connection the API dropped or reset before it answered,
  is asked again as a 5xx was, each wait less up to half; a 501 isn't. 429s
  and 5xx are counted apart. An answer too large to send is halved as
  before, however long the API's text that says so.
- **Five minutes at most, and within `--timeout`.** A request is asked again
  for five minutes at most from when it was first pushed back, by an answer
  or by a wait for its turn the API asked for; the API's answers and the
  waits for a turn count. A request whose turn doesn't come by then is given
  up with the last answer; one already asked is waited for. A wait that
  would end past `--timeout`, the API's or a turn by `--max-rate`, isn't
  waited for: the rule is left unfinished at once, as the deadline would
  leave it, and the run exits 1 saying to raise `--timeout`; `lapilli
  serve`'s MCP compare says so too.
- **Fewer at once after a push back.** A 429, a 503 or a connection dropped
  halves how many requests may go to that API at once: half of those in
  flight lately, or of the limit, if fewer, from 64, once a second at most,
  for the whole run (or, in `lapilli serve`, from then on), down to one;
  eight at once are four after one push back. One more may go for every
  ten seconds without a push back, asked or not. While it said to wait
  (`Retry-After`), nothing else is asked of it, a request already waiting
  for its turn included. Rules sharing a query still share one request.
- **`--parallel` is of each API.** Up to N rules are replayed at once against
  each: `--prometheus`, `--loki`, each of Grafana's data sources. Rules held
  by one API that pushes back no longer hold those of another. A Grafana is
  asked N requests at once at most for all its data sources, as one
  Grafana answers them all.
- Behind one Grafana, each data source backs off of its own, as Grafana's
  data source proxy passes on what each answers, `Retry-After` included;
  they share `--max-rate`.
- The same holds for every API history is read from: Prometheus, Thanos,
  Mimir, Cortex, VictoriaMetrics, each cloud's managed Prometheus and
  CloudWatch through `--auth`, Loki, and Grafana's data source proxy; and
  for the rules read from the same API (`--loaded-rules`, where a Loki's
  ruler runs its groups). What lapilli does without, the configuration and
  the targets Prometheus is asked for, and where a Loki's ruler runs its
  groups as `lapilli serve` reads it each minute, is asked once, within 30
  seconds, not while the API said to wait, and its answers don't change how
  the rest is asked, nor count as answers of HTTP 429 at a run's end.
  `lapilli serve` keeps where the groups run as last read when it can't be
  read, so a Loki pushing back no longer stops the rules being read. A
  request to a Loki, or to a Grafana's data source, is given up after five
  minutes, as one to a Prometheus was.
- A rule whose replay failed for the run is named, with why, at the top of
  the text report and of a comparison, ten rules at most, as well as at its
  foot. A failed query says how often it was asked, and over how long.
  `lapilli serve` keeps the reason in its ledger without that, and its
  weekly digest, whose numbers keep what a rule fired before its replay
  failed, names no rule at its top.
- The API's text in an error is cut to 300 bytes, and in a top line to 400,
  each with its "…"; what doesn't show (control and format characters) is
  taken out first, then the token, the signature or the URL's password
  lapilli sent, and any bearer token, should the API echo them back, split
  or not; "bearer" before a word, as in "missing bearer token", is left. A
  sample the API writes unreadably is quoted in the error.

## [0.32.0] - 2026-11-13

Pull request backtests in CI, on every code host: what a team had to glue
around `lapilli verdict --compare` to comment its report on a pull request
that changes alert rules, built in. Reviewed as an attacker would and for
correctness before release; what both found is fixed in what is described
here. No ledger schema change; the exit statuses, and every field `--json`
gave, are as they were.

### Added

- **Pull request backtests in CI** ([recipes/backtest](recipes/backtest)):
  a script for each code host, GitHub, GitLab, Bitbucket, Azure DevOps and
  Gitea (or Forgejo), one POSIX sh library under them, and for GitHub
  Actions an action (`uses: lapilli-project/lapilli/recipes/backtest@...`).
  Each installs a pinned release, its archive checked against the
  release's `SHA256SUMS` (and `LAPILLI_SHA256`, which a mirror needs)
  before it is unpacked; reads the rules of the commit the pull request
  goes into and of its own from git, as files (`PATHS`) or as a command
  renders them in each (`RENDER`, `RENDER_PREPARE`: a Helm chart, a
  Kustomize overlay), the base's first, each with a `HOME` of its own and
  without the variables CIs keep tokens in; reads the incidents from the
  commit the pull request goes into (`INCIDENTS`), never the pull
  request's, and refuses a file an argument names anywhere in the
  checkout; takes the branch a pull request goes into from the clone when
  the checkout fetched it, so a checkout without credentials does; runs
  `lapilli verdict --compare --changed-only --markdown`; and posts one
  comment, found again by a hidden first line and the token's own account
  (on Bitbucket, `BITBUCKET_USER_UUID`), and edited on later runs. Nothing
  changed and no comment yet: none posted. What fails is said in the
  comment, in a fenced block, nothing of the pull request's outside it,
  taken out where the history is before it is cut, never why a rule
  couldn't be replayed or what a server or the network said; the job
  succeeds unless `FAIL_ON_ERROR=true`, which a rule that couldn't be
  replayed fails too. A `PATHS` neither commit has fails the job. What it
  copies to the job's log is each line after a `| `, so none of it is a
  CI's command. Answers are capped whatever curl's version. Tested against
  a stand-in for each host's API, and end to end against a real Gitea and
  Prometheus (`test/e2e/backtest.sh`, in CI): a comment posted, edited
  after a push, none on a pull request that changes no rule, a rule named
  to mention, reference and break the table rendered by Gitea itself as
  text, a link whose path mentions a user refused without notifying
  anyone, from a checkout without credentials.
- **`lapilli verdict --compare --markdown`**: the report as Markdown to
  post on a pull request or a merge request: a heading with the period and
  its time zone, the totals, the rules that differ as a table, the
  recording rules that differ, those that couldn't be replayed (a line a
  rule), those that fire as they did folded in `<details>`, the incidents
  caught differently, what reading the files passed over; at most 64,000
  bytes, cut short with a note that counts rules. Rule names, labels,
  incidents and reasons are written as code spans, so none can format,
  link, mention (`@user`, `@org/team`), reference (`#123`), break a table
  or hide what follows; a name is cut before anything is made of it. A
  query that failed for want of reaching the history (anything but the API
  itself refusing it, 400 or 422 in its own words, not a proxy's page) says
  only that, its reason going to stderr; from the rest, the URLs, user
  names, hosts (each as a whole word, a full stop after it too), IP
  addresses, tenants and project the history is read with are taken out. When nothing changed, a short form that doesn't
  change. It doesn't go with `--check-content`.
- **A summary for tools.** `--json` with `--compare` adds `"summary"`:
  `nothing_changed`, `changed`, `added`, `removed`, `same`, `alike`,
  `unreplayed`, `recording_changed`, `firings_before`, `firings_after`,
  `incidents_given`, `incidents` and `incidents_changed`, each always
  there; `--markdown` holds the same on its first line. "No incidents
  given" is `incidents_given: false`, not zero caught. Nothing changing has
  no exit status of its own (the README says why).
- **Recording rules in a comparison.** `--compare` reads the recording rules
  beside the alert rules, wherever Prometheus's groups are, in Azure rule
  groups and in Grafana provisioning files, and one that differs
  (`"recording"` in `--json`) is a change: `nothing_changed` is false. A
  replay reads what a recording rule recorded, from the history, so the
  report says the alerts that read one are replayed against that, not what
  the change would record.

### Changed

- **`--compare` runs without incidents.** Without `--incidents` or
  `--impact` the firings are counted, not judged: the report leaves out the
  caught, nothing and precision columns and what it would say of incidents,
  and keeps fired and re-fired. A replay without `--compare` still asks for
  them.
- **A Prometheus group's `labels` and `limit` are each of its rules'.** The
  alerts a replay gives carry its labels under the rule's own, as
  Prometheus 3 does, and a change to either is a change to its rules, in a
  comparison and as a new version in lapilli serve. A limit isn't replayed
  (past it, Prometheus drops all of a rule's alerts): a rule under one is
  marked replayed in part. A rule's name, and in lapilli serve its
  versions, history and proposals, are as they were: an upgrade makes no
  new version, and a change proposed before it goes on (tested on a ledger
  v0.31.2 wrote). The rules API's group `limit` is read too.
- **A Grafana recording rule is no alert rule.** One with `record`, in a
  provisioning file, was read as an alert rule; it is now a recording rule,
  compared as one. lapilli serve, reading a Grafana with recording rules,
  stops listing them among its alert rules: one new version, on upgrade.
- A report's header names its time zone with each offset the period took,
  `Asia/Seoul (+09:00)`, `Europe/Berlin (+01:00 → +02:00 → +01:00)`, or
  `UTC` by any of its names (`Etc/UTC`, `Etc/Zulu`), never Go's bare
  `Local`: the machine's own zone is named as `TZ` or `/etc/localtime`
  says, through every link; a zone file copied, by its abbreviations. `--tz`
  takes an IANA name, as it did. lapilli serve's digest header too.
- With `--compare`, what reading the rule files passed over is said for
  each set apart: "the rule files --rules read held ..." and "the rule
  files --compare read held ...", not the same line twice.
- `lapilli verdict`'s errors show a line break, or any control character,
  in what a rule file or a flag holds as `\u{...}`, and the names of rules,
  groups, alarms and namespaces quoted: no line of an error is one of its
  own, which a CI could read as a command.
- A file of incidents is read no further than 64 MiB.

## [0.31.2] - 2026-11-13

SQLite 3.54.1, held back since 0.20.1, and a week of firings written
faster for it. No ledger schema change.

### Changed

- **SQLite 3.54.1** (`modernc.org/sqlite` v1.62.0, from v1.60.1 and
  SQLite 3.53.4). 0.20.1 held it back: with v1.61.0 a week of 100,000
  firings copied in was written a fifth slower. The driver was why, not
  SQLite or the ledger. It cleared a prepared statement's arguments after
  each run, and SQLite 3.54 then planned a statement with a `col = ?` term
  and an `ORDER BY` again at its next run, as it does the query for an
  alert's firing before those being judged: run once for each alert,
  about 99,000 times for that week, at twice the cost. v1.61.1 keeps the
  arguments in place (https://gitlab.com/cznic/sqlite/-/issues/264). With
  v1.62.0 the week is written in 1.98 s, against 2.15 s with v1.60.1 and
  2.68 s with v1.61.0 (medians of 20 runs on a laptop), and twelve weeks
  are backfilled a tenth faster. No ledger schema change.

## [0.31.1] - 2026-11-12

Two fixes the new end-to-end run found (`test/e2e/service.sh`: the whole
service on kind, with Prometheus, Grafana sending its notifications to
lapilli, Dex and a load balancer, and a browser). No ledger schema change.

### Fixed

- **`lapilli serve` didn't start with a relative `--ledger`** (since
  0.29.0), its default `lapilli-ledger` and the README's first example
  among them: written as a URI, the path's first directory was read as a
  host, and SQLite refused it (`invalid uri authority`). The path is made
  absolute first, and a Windows path keeps its drive in the path. The
  chart, which gives an absolute one, wasn't affected.
- A team or a comment written through the API was answered as it was
  sent, not as it was kept: no author, a time of 0001-01-01, a team's name
  untrimmed. It is answered as `GET` reads it back, as a quiet window, a
  task, a report and a role already were.

## [0.31.0] - 2026-11-12

What an operations team asks of its alerts (docs/design/noc.md): re-fires,
firings during deploys, user impact read as incidents, week by week and team
by team, in the UI, the digest, `/metrics`, a new summary API and the
chart's Grafana dashboard. **Ledger schema 017**, after 0.28.1's 016: tables
of deploys and of their label sets (each kept once), a mark on each firing
that came during one, counted by day and week, and an index of re-fired
firings. No change to how a firing is judged. Reviewed before release as an
attacker would and for correctness; what both found is fixed in what is
described here (docs/design/noc.md, "Settled in review").

### Added

- **Re-fires, read as a goal.** `/rules` says the share of each rule's
  firings that re-fired and in how many of the whole weeks shown one did,
  and can be put in order of re-fires (`?sort=refired`); a rule's page has
  its re-fires week by week and the alerts (label sets) that re-fired most;
  the overview a tile and a column; the digest and a report's new `refired`
  section, how many re-fired, of how many rules, the alert that did most,
  and how many of the last twelve whole weeks had one. "Whole weeks" is one
  thing everywhere: neither the week under way nor the one the ledger's
  first firing fell in, with how many were counted beside it. `/metrics`
  adds `lapilli_rule_refired_weeks`, `lapilli_refired_weeks` (everyone's and
  each team's) and `lapilli_weeks_counted`.
- **Firings during deploys.** `--deploys FILE` (the chart's `serve.deploys`)
  names what was deployed and when, as the histories
  `recipes/versions/argocd.sh` and `flux.sh` write (regular files beside it
  or below it), each labelled with the service or namespace it deployed, or
  written down with a start and an end; read again each run, and kept again
  only when it or a history changed (sha256). A firing that starts during
  one, from `--deploy-before` (0) before its start to `--deploy-after` (10m)
  after its end, and agrees with it on every label both have, one at least
  (a namespace's own alert during a service's deploy in it, not another
  service's), is marked: judged as ever, shown apart, and precision read
  with those firings and without, on `/rules` (a column and
  `?sort=deploys`), a rule's page, the overview, the digest, a report's
  `deploys` section, `/metrics` (`lapilli_rule_firings_during_deploys`,
  `lapilli_rule_caught_during_deploys`,
  `lapilli_serve_deploys_last_timestamp_seconds`,
  `lapilli_serve_deploys_read_timestamp_seconds`) and the summary. Bounded:
  50,000 deploys, 8 MB of labels in all, none starting more than an hour
  after the file is read. Without `--deploys`, a ledger that keeps deploys
  says so on the status (a file of `deploys: []` takes them away).
- **Impact windows read as incidents.** `/incidents` picks written incidents
  or windows `--impact` found (`?source=`) and a team's (`?team=`), says how
  many were caught, caught only by noisy rules or missed and the median
  lead, and gives each its lead and, for one not caught, the rule that nearly
  caught it. `/metrics` has `lapilli_incident_state` and
  `lapilli_incident_lead_seconds`, an incident a series (the latest 500), and
  `lapilli_incident_team`, which teams own each (500 series at most). An
  incident's state is judged by the rules' stones in its week everywhere,
  `/metrics` and so the dashboard included.
- **`GET /api/v1/summary`**, and MCP's `summary` tool: for each week and for
  everyone or each team, critical firings and the share that caught
  something (`?critical=`), by severity, re-fires and the alert that
  re-fired most, off hours and asleep, during deploys and precision without
  them, incidents and impact windows caught and missed, and the median lead.
  `schema: 1`: fields are added to it, never changed. Bounded: 53 weeks, each
  team once, 32 critical severities; one of every team, or of more than 53
  rows, takes the one heavy turn, through the API and MCP alike.
- **The chart's Grafana dashboard rebuilt** around that evaluation, with
  variables for the team, severities and the severities counted as critical
  (several at once, a rule whose versions' severities differ included), and
  the sources' health: how long since firings were copied in, a
  notification came, the deploys file was read, the ledger's runs, the
  sources gone quiet, failed runs. `test/e2e/dashboard.sh` asks every query
  of a real Grafana 13.3.1 over a real Prometheus 3.5.0 scraping a real
  lapilli serve, in CI too. `/metrics` adds
  `lapilli_serve_notifications_last_timestamp_seconds` and
  `lapilli_serve_source_quiet_seconds`. The chart's ServiceMonitor honours
  lapilli's labels and takes `serve.serviceMonitor.sampleLimit`.
- **Runbooks where teams keep them.** `--runbook-annotations` (the chart's
  `serve.checkContent.runbookAnnotations`) names the annotations a runbook is
  read from, the first a rule has: `runbook_url`, `runbook`, `playbook_url`
  and `playbook` by default, kept first past 32 annotations. A path in the
  rules' repository, with an anchor or not (`docs/runbooks/disk.md#full`),
  is a runbook, shown as a path, or a link under `--runbook-base URL` (the
  chart's `serve.checkContent.runbookBase`); what teams write in a runbook's
  place (`N/A`, `TODO.`, `...`) isn't one.

### Changed

- The rule metrics (`lapilli_rule_firings` and the rest, from lapilli serve)
  carry `team` when a team owns the rule. A query summing by `rule` and
  `severity` reads as before; a rule's series changes when its team does.
- The content check's `runbook_not_url` says "a runbook that isn't a link: an
  http or https URL, or a path in a repository"; a path is no longer one. A
  link with a user name (`https://wiki.example.com@evil.example/`) is one,
  and is never shown as a link.
- Before its first run, `/metrics` gives no `lapilli_last_run_timestamp_seconds`
  or `lapilli_serve_cursor_timestamp_seconds` (they were 0, or the year 1).

## [0.30.0] - 2026-11-12

lapilli serve served the way Grafana and Argo CD are, at a URL people
open, behind a load balancer, an ingress or a Gateway, signing them in
itself ([docs/design/signin.md](docs/design/signin.md)). Reviewed from an
attacker's point of view and for correctness before release. No ledger
schema change.

### Added

- **A sign-in of its own, with OpenID Connect.** `--oidc-issuer`,
  `--oidc-client-id` and `--oidc-client-secret-file` sign people in with any
  provider (Google, Microsoft Entra ID, Okta, Keycloak, Dex, AWS Cognito, an
  OCI IAM identity domain, GitLab...): the authorization code flow with
  PKCE, state and nonce; the ID token's signature, issuer, audience, `azp`
  (`--oidc-trusted-clients`), expiry and nonce checked, Google's `hd` with
  `--oidc-hosted-domain`, and `email_verified` asked of an `email` name. A
  browser keeps a session in a signed cookie (`__Host-`, HttpOnly, Secure,
  SameSite=Lax; `--session-max-age`, 12h; signed with
  `--session-key-file`, or a key made for the process: never kept in the
  ledger). `POST /auth/logout` ends every session of that person made
  until then, in every browser, and every token issued before, kept in the
  ledger. An API client or an agent sends an ID token the provider gave the
  client as a bearer token (`--oidc-bearer=false` takes none); it never
  approves a change, and a refused one gets `error="invalid_token"` with
  the reason. `/.well-known/oauth-protected-resource` (RFC 9728) names the
  provider. `--user-claim` (`email`) and `--groups-claim` (`groups`) give the
  roles.
- **Only those a role names come in.** An identity provider signs in
  whoever it knows, which for a public one (Google's for any account,
  GitLab.com, GitHub through Dex, a multi-tenant Entra ID app) is anyone.
  With a sign-in of lapilli's own, every page and API call but the health
  checks needs someone signed in whom `--viewers`, `--editors`, `--admins`,
  `--approvers` or a role given in the UI names; everyone else gets 403.
  Lists take people, `group:NAME` and `*@DOMAIN`; `'*'` gives a role only
  once `--viewers` says who comes in. `--viewers` works with
  `--user-header` too. There is no anonymous viewing with a sign-in.
- **A sign-in in front, by the header it signs.** `--proxy-jwt google-iap`
  (Google Cloud's Identity-Aware Proxy), `aws-alb` (an Application Load
  Balancer's sign-in) or `jwks` (oauth2-proxy, an Azure App Service's
  sign-in, an access proxy) takes who each request comes from from the JWT
  that sign-in puts in a header, checked against its keys on every
  request, in place of a plain header only a NetworkPolicy keeps others
  from setting. Keys are fetched only from the URLs given, or the hosts a
  load balancer's ARN names, a key not held at most once a minute, a load
  balancer's from a budget of its own, and a key id it doesn't have
  remembered.
- **The chart serves the UI at a URL.** `serve.service.type` puts the UI
  alone on `FULLNAME-ui`, a LoadBalancer or a NodePort (`port`,
  `annotations`, `loadBalancerClass`, `loadBalancerSourceRanges`,
  `externalTrafficPolicy`, `nodePort`); `FULLNAME` stays a ClusterIP with
  every port. `serve.ingress` (networking.k8s.io/v1) and `serve.httpRoute`
  (Gateway API), whose hosts become `--external-url` (https unless their
  `https` says not) and `--allowed-hosts`. `serve.signIn` gives the
  sign-in's flags and mounts its Secrets, and makes the session key's
  Secret and keeps it. `serve.roles.viewers`. The chart's README has each
  cloud's load balancer, GKE, EKS, AKS and OKE alike.
- `test/e2e/signin.sh`, against a real Dex in Docker, and
  `test/e2e/serve-lb.sh`, the chart on kind behind a LoadBalancer, a
  Gateway and an Ingress (cloud-provider-kind) with Dex in the cluster and
  the NetworkPolicy on: both in CI.

### Changed

- **`/readyz` is ready once the ledger has a run recorded**: at once after
  a restart, after the backfill's first day on a first install. Before, it
  waited for the process's first round of jobs, up to `--timeout` (the
  chart's 50 minutes) with a slow Prometheus. The chart's readiness probe
  is now `/readyz` (it was `/healthz`), so `helm install --wait`, `helm
  upgrade --wait` and Argo CD's health wait for that on a first install;
  `serve.readinessPath: /healthz` is ready at once, as before. The
  liveness probe stays `/healthz`.
- With `--oidc-issuer` or `--proxy-jwt`, loopback makes no one an admin,
  and `--everyone-admin` is refused. `--user-header` keeps the 0.28.1
  rule.
- A name from a header or a claim with spaces around it, or spacing other
  than the ASCII space (U+00A0, U+3000), is no one's, as one with a
  character that doesn't show already was.
- The chart refuses to serve the UI outside the cluster (a LoadBalancer or
  NodePort, an Ingress, an HTTPRoute) with no sign-in, unless
  `serve.exposeWithoutSignIn`; `serve.userHeader` on a LoadBalancer or a
  NodePort, unless `loadBalancerSourceRanges` names the proxy's addresses
  and `serve.service.sourceRangesAreTheProxy` says so; and, with
  `serve.signIn.oidc`, an http URL but to this machine. `serve.roles` and
  `serve.changes` take `serve.signIn` as they take `serve.userHeader`.
  `--external-url` and `serve.ingress.path` take no path but `/`.
- Upgrading the chart with `--reuse-values` from 0.28.1 works: the values
  added since are filled in where missing.
- New dependencies: `github.com/coreos/go-oidc/v3` v3.21.0 and
  `github.com/go-jose/go-jose/v4` v4.1.6, both Apache-2.0.

## [0.29.0] - 2026-11-12

Rules a repository renders, followed (docs/design/history.md). Reviewed
before release, as an attacker and for correctness; what both found is
fixed in what is described here. No ledger schema change.

### Added

- **Rules read out of rendered manifests.** `--rules`, `--rules-history`
  and `--compare` take a chart rendered whole (`helm template`, `kustomize
  build`, `kubectl get -o yaml`): of its documents, those that hold rules
  (ConfigMaps of rule files or of Grafana provisioning files,
  `PrometheusRule`s and the rest Lapilli reads, `List`s of them) are kept,
  and the others passed over and counted: `verdict` says how many on
  stderr, `serve` in its log. No `yq` is needed to filter them first.
- **`--rules-select SEL`,** a label selector as `kubectl -l` takes one
  (`key=value`, `!=`, `in`, `notin`, `key`, `!key`, `key>N`, `key<N`),
  keeps of the Kubernetes objects in the rule files those it matches:
  `grafana_alert=1` for the ConfigMaps Grafana's sidecar loads. A set with
  an empty value (`a in (x,)`), which kubectl takes, is refused as a slip.
  Labels are on objects only: a plain rule file has none, and is kept.
- **What holds rules, read closely.** A kind's own list (`ConfigMapList`,
  `PrometheusRuleList`) is read as `List` is. A Kubernetes object of a kind
  rules aren't kept in holds none, whatever else it has (a Secret with a
  top-level `groups`). A ConfigMap whose rule file doesn't parse is counted
  and said apart from what holds no rules. A file of rules is read 16 MiB
  at most.
- **Rendering each commit reliably.** `recipes/versions/git.sh` takes
  `RENDER_PREPARE`, run in each commit's files before `RENDER`, for what a
  commit lacks, such as a chart's dependencies that older commits don't
  vendor (`helm dependency build`, at the versions `Chart.lock` pins, or
  the archives as the newest commit vendors them, from `REPO`). It runs as
  `RENDER` does: without the token, with a `HOME` of its own, kept from
  commit to commit. `UNRENDERED=skip` passes over a commit that can't be
  rendered (`RENDER_PREPARE` or `RENDER` fails, or `RENDER` writes
  nothing) or read (below), saying which and why; by default it still
  stops, now saying which commit and that `skip` exists. Kustomize is
  documented. `MAX_TREE_FILES` (20000) and `MAX_TREE_BYTES` (256 MiB)
  bound what a commit is read with, by `git ls-tree` before anything is
  written.
- **Following the branch.** Each version `git.sh` writes holds `.commit`,
  its commit; `AFTER=COMMIT` writes only the commits after it.
  `recipes/versions/follow.sh` keeps a history up to date: at each look,
  the commits since the newest version's, added beside it, and with
  `KEEP`, the versions older than that taken away but the one in effect
  then. It never stops on a commit (`FOLLOWING`): one that can't be
  rendered or read is passed over, one dated after now is named now, of
  more than `MAX` new commits the newest are written, and `.head` keeps a
  commit passed over from being rendered again. A commit is named no
  earlier than when the clone's branch first held it, by the branch's
  reflog, less `LATE` (`EVERY` and a minute): one dated back takes effect
  when it was fetched. `lapilli serve` given `--rules-history` without
  `--rules` follows the directory: its newest version that reads is the
  rules in effect, and each version added later is recorded from its
  time, with what is read beside the rules (`--alarms`, `--loki-rules`),
  the hours since then replayed again by the next run (no further back
  than `--backfill`) and their days checked again. A version that can't
  be read is passed over, once, saying so; following never stops on it.
  `--changes` is refused when the rules are a followed history alone.
- **In the chart,** `serve.rulesHistory` takes `renderPrepare`,
  `unrendered` (`skip` by default), `select`, `max`, `every`,
  `maxTreeFiles`, `maxTreeBytes` and `sizeLimits`. With `render`, the
  versions are written in `renderImage`, an image with git, helm and
  kustomize, pinned by digest: Argo CD v3.5.6's by default (about 210 MB
  to pull); the README says how to give a smaller one. With no
  `rules.configMaps` the repository is the rules, followed:
  `rules-history-fetch` fetches the branch every `every` seconds (300)
  with the token, and `rules-history-follow`, without it, adds the new
  commits' versions, keeping the backfill and a day. When the start
  writes no version, serve starts all the same, and takes the history once
  it is fetched and written.
- `test/e2e/rendered.sh`, in CI: a real Gitea holding a chart of Grafana
  rules whose subchart comes from Gitea's Helm registry and isn't vendored
  at the first commit; the chart's own rules history containers, as `helm
  template` writes them, run in Docker; each version read for exactly its
  rule documents; `verdict --rules-history` and `serve`, following, against
  a real Prometheus with three days of a series, each day replayed with
  that day's rules; and a commit dated hours back, fetched, taking effect
  when it was fetched. `FuzzPick` joins the weekly fuzzing, and checks
  that nothing but documents of rules is kept.

### Changed

- `lapilli serve` keeps of each version in `--rules-history` only its
  documents of rules: a chart's Secrets and Deployments never reach the
  ledger.
- `lapilli serve --rules-history DIR` without `--rules` was refused; it now
  follows the directory.
- A version in `--rules-history` that can't be read no longer stops a
  first start: it is passed over, saying so, and the version before it
  stays in effect.

### Fixed

- **A ledger whose directory's name holds `#` or `?` opened another file**
  (main since 0.6.0): the path was cut there, so two ledgers named alike
  before it were one. The path is escaped; `%` and spaces read as they
  are too.
- **Following, found in review:** a version recorded while a run, or a
  check, begun before it was under way was lost (each note now bumps a
  sequence, and a run or a check forgets only what it saw); a good version
  found with a bad one after it in one look wasn't replayed again; sending
  the check back left what it kept of those days (a miss kept twice, a
  rule the version dropped keeping its checks); and beside `--alarms` or
  `--loki-rules`, every later commit took effect when seen.

### Security

- **In the chart, the clone is read-only where `render` runs.** It moved to
  a volume of its own (`rules-history-repo`) that only the containers
  holding the token write (`rules-history-clone`, and following,
  `rules-history-fetch`): `rules-history` and `rules-history-follow` mount
  it read-only, so nothing `render` runs can change the configuration or
  hooks the next fetch runs with. EKS's and Azure's webhooks pass the new
  containers over too, and the service account's token is mounted in none
  of them.
- **A commit is read as it is.** git runs with none of the image's or the
  user's configuration (the render image turns Git LFS on, its filter
  required, its server the commit's `.lfsconfig`), and from an empty work
  tree and index, so a commit's `.gitattributes` don't leave a rule file
  out (`export-ignore`), rewrite one (`export-subst`) or run a filter.
  With `RENDER`, a symbolic link anywhere in the commit, or one
  `RENDER_PREPARE` leaves, can't be rendered: a `file://` dependency's
  link outside `PATHS` carried what it led to (the container's
  environment) into a selected rule's annotation, and the ledger.
- **A commit's date can't rewrite the ledger.** Following, a version's
  time is bounded below by when it was fetched (above): a commit dated
  back had every hour since its date, up to `--backfill`, replayed and
  judged with rules not in effect then.
- **Bounded disk and memory.** What a commit is read with is bounded
  (`MAX_TREE_FILES`, `MAX_TREE_BYTES`): one blob named many times in a tree
  was gigabytes written out. The chart's volumes have size limits, its
  history containers ephemeral-storage requests and limits, and a
  followed directory keeps the backfill and a day.

## [0.28.1] - 2026-11-12

A security release, after two reviews of 0.28.0 from an attacker's point
of view, and a third of the fixes: the change recipes, the chart, the
release workflow, and what lapilli serve takes. Ledger schema 016 (tasks'
ids), applied at start.

### Changed (security)

- **Without roles, a server listening beyond loopback takes no changes.**
  With no `--editors` or `--admins`, everyone was an admin of the UI and the
  API wherever lapilli serve listened. The chart listens on `:8080` with no
  roles by default, so any pod in the cluster could write incidents, tasks,
  quiet windows and the settings (medium). Now, with no roles, everyone is
  an admin only on a listener on loopback alone (`127.0.0.1`, `::1`,
  `localhost`, the binary's default), as `--listen` says and the address
  it is bound to is; beyond it everyone may only look, and the log says so
  at start. To write: give roles behind a sign-in
  proxy (`--user-header` with `--editors` or `--admins`), or, to keep what
  0.28.0 did, `--everyone-admin`, in the chart `serve.everyoneAdmin: true`
  (off by default). Chart users with no `serve.roles` who wrote through
  `kubectl port-forward` need one of the two after upgrading.
- **The change recipes' branches name the ledger.** `lapilli/LEDGER/proposal-ID`,
  LEDGER twelve hex digits the ledger makes once and the changes API hands
  out, so two ledgers proposing to one repository never share a branch. A
  pull request already open is followed as before, by its link. A change
  still waiting for one goes on the new branch; a `lapilli/proposal-ID` a
  recipe before 0.28.1 pushed for it is taken when it holds the same
  change, and otherwise left as it is, which the run says (close its pull
  request by hand). A 0.28.1 recipe with an older Lapilli, which names no
  ledger, keeps `lapilli/proposal-ID`.
- **Release tags are `vX.Y.Z` on main.** Others (`v1.0.0-rc.1`, a tag off
  main) no longer build. Releases publish one at a time, and `latest`, the
  image's and GitHub's, is always the highest version released.

### Fixed (security)

- **A Lapilli someone took over could have a recipe push a CI file
  (high).** `docs/design/changes.md` assumes a stolen Lapilli: it hands the
  recipes a document and its hash. Any file under `RULES_PATHS` with that
  hash was written and pushed, but for a list of CI systems' names in one
  letter case: an executable script (its mode kept), a `Makefile`, a GitLab
  `include: local:` file, a build's configuration, a case variant of
  `.gitlab-ci.yml`, or `RULES_PATHS` itself naming one. CI that runs on a
  branch's push would run it with the repository's secrets before anyone
  reviewed it. The recipes now write only to a file of rules: mode 100644,
  named `*.yml`, `*.yaml`, `*.json` or `*.rules`, not what a CI system runs
  (a longer list, any letter case, `RULES_PATHS` too), one document of it
  at least a Prometheus rule file, a Grafana provisioning file, a
  `PrometheusRule` or a `ConfigMap`, and each document read as YAML reads
  it. And in it they change only the rules: each document as it was, or,
  rules in both, the same but for a rule file's `groups`, a
  `PrometheusRule`'s `spec`, or a `ConfigMap`'s entries, each changed one
  a file of rules alone. Anything else fails, with the reason on the
  proposal's page. (The review of the fixes found a document indented as
  a whole, a `ClusterRoleBinding` after a `PrometheusRule`, taken unseen,
  and a ConfigMap of rules rewritten as `aws-auth`, or a Grafana file
  given contact points, taken for their shape; and real files refused: a
  `ServiceMonitor` beside its `PrometheusRule`, a byte order mark, a
  `ConfigMap` with `immutable`. All fixed before release.)
- **A reviewed pull request could be changed under its approvals
  (medium).** The recipes pushed their branch with `--force` and took the
  pull request open from it: a stolen Lapilli handing an opened change out
  again as ready, or two ledgers with the same proposal id on one
  repository, rewrote the branch, keeping its approvals and auto-merge.
  Now a branch is pushed only where there is none. One holding the same
  change is a stopped run's, and taken; one holding another fails the
  change, saying why.
- **A team's editor could slow judging for everyone (medium).** Quiet
  windows had no bound, and each was set beside every firing it might
  excuse inside the ledger's one write transaction: 1,500 of a team's took
  re-judging 20,000 firings to about 106 s. The ledger now keeps 500
  windows, the last 50 for editors and admins to make, and a team's
  editors keep 20 of its windows, wherever they move them. What may
  excuse a firing is worked out once a span: by rule and team, and, for a
  window repeating by its schedule, the times it falls due in the span,
  which each firing is set beside by a search. A day's 10,000 firings
  recorded beside 500 windows that fall due in it take 0.63 s (0.34 s
  beside none; 25.9 s before), and judged again beside 500 that don't,
  58 ms (9.5 s before).
- **The release job held every write permission while it ran the
  repository's code (medium),** and restored a Go cache main's CI wrote.
  `release.yml` is now three jobs: `build`, which may write nothing and
  restores no cache (`go mod verify` before building); `verify`; and
  `publish`, which runs none of the repository's code, checks each file's
  sha256, and alone has the write permissions. The assets, attestations,
  Sigstore bundles and immutability are as before. A dry run
  (`gh workflow run release.yml --ref main -f version=vX.Y.Z`) builds and
  verifies and never publishes. Helm is pinned by version and sha256,
  BuildKit by digest, and `latest` is tagged only once the release is out,
  one release at a time, and only to the highest version.
- **`--prometheus` followed redirects (low).** Whatever answered as the
  Prometheus could send the query elsewhere, a bearer token to the same
  host's other ports, and with `--auth aws` the session token to any host.
  It now follows none, as every other client already did, and gives up on
  a request after five minutes.
- **A deleted task's id was given again (low).** The next task, maybe
  another team's, took over the deleted one's tracker issue: its comments
  both ways, and its close. Schema 016 makes task ids never repeat, and a
  proposal made into a deleted task is made into none. Upgrading, new ids
  start past the highest the ledger still knows was given (its tasks, the
  changes it keeps noted of tasks, the proposals made into them): a task
  deleted with none made after it, whose changes `--keep` has dropped
  since, leaves no trace, and its id can be given once more, its issue
  taken. The tracker recipes don't yet hold an issue to its task's time
  (`docs/design/trackers.md`).
- **The digest's preview skipped the one-heavy-request-at-a-time turn
  (low),** a regression of 0.25: any viewer could ask for many at once. It
  now waits its turn, as a report's preview does.
- **A recipe run pushed as many branches as it was handed (low).** Now
  `MAX_CHANGES`, 10 by default; the rest wait, and the run says so.
- **What a server answered reached a recipe's log raw (low),** escape
  sequences and all. It is now shown as a JSON string.
- **A token alone as `CLONE`'s user passed (low),** into git's argv and
  `.git/config`. Any user in an http or https `CLONE` is now refused.
- **The chart's NetworkPolicy limited only what comes in (low).**
  `serve.networkPolicy.egress` limits where the pod may connect, the
  cluster's DNS and what `to` names; off by default.
- **A cloud's identity reached the rules history's init containers
  (low),** which run git and `render` on a repository's content. The pod
  no longer mounts the service account's token anywhere but lapilli's own
  container (and there only for a cloud's identity), and EKS's and Azure's
  webhooks pass the init containers over. GKE's metadata server answers
  every container; the chart's README says what that means.
- **The chart ran its image by tag (low),** which can be moved. A
  release's chart now runs its image by digest (`image.digest`), and
  refuses another tag kept with its own digest.
- **`serve.trackers.image` took a tag (low),** though the docs asked for a
  digest. It must be `NAME@sha256:...` now.

### Documented

- `--mcp-compare` also sends the LogQL an agent wrote, for a rule kept in
  Loki, with Lapilli's token.
- Grafana's writes are scoped by the token alone: what an Editor's, limited
  by folder, and an Admin's, needed to turn a rule off, reach.
- Azure's metric and activity log alerts have no etag to write against:
  what may be written over between Lapilli's read and its write.

## [0.28.0] - 2026-11-11

Rules written in LogQL, replayed against Loki (docs/design/loki.md); the UI
in Spanish and Japanese; an accessibility review of the UI and its charts;
provenance beside each release; and the tests' data races gone
(docs/design/access.md). Reviewed before release, as an attacker and for
correctness; what both found is fixed in what is described here. No
ledger schema change: 0.27's 015 stays.

### Added

- **Loki's ruler's rules, replayed.** `--loki URL` (with
  `--loki-token-file` and `--loki-tenant`) is a Loki that rules written in
  LogQL are asked of, through its `query_range`. `--loki-rules PATH` reads
  a Loki ruler's rule files, `--loki-loaded-rules` the rules the Loki
  holds (`/loki/api/v1/rules`), each group placed where Loki runs it
  (`/prometheus/api/v1/rules`); beside any other source of rules, or
  alone. `lapilli verdict` needs `--prometheus` beside them only for what
  reads metrics; `lapilli serve` always needs it. A rule file's
  `query_offset` and `keep_firing_for`, which Loki 3.7.9 drops and doesn't
  run, are dropped too, and said; what Loki's API gives is trusted.
  `--check-replay` reads the `ALERTS` Loki's ruler writes through its
  `remote_write`. A Loki rule sharing a name is told apart as
  `Name [loki:namespace/group]`, and a change proposed for one, with its
  backtest, is for a person: Lapilli doesn't write to Loki.
- **Grafana's rules on Loki, replayed.** With `--grafana`, the type of each
  data source a rule reads is asked of Grafana; a Loki one's queries go
  through Grafana's data source proxy, with the rules' token, by GET: the
  Loki, tenant and credentials Grafana used. Instant and range queries at
  Grafana's step (a query's own step times its resolution; else its
  `intervalMs`, at least the window over 11,000), its Loki macros
  (`$__auto` the step, `$__interval` the query's interval, `$__range`),
  and its reducers over a range, worked out by Lapilli a batch at a time,
  since LogQL has no subqueries; no data told from no history as for
  PromQL. Read from files, a query whose model says `datasource.type: loki`
  is asked of `--loki`.
- **Said, not replayed:** a log query; a reduce asking Loki finer than a
  point a second (86,400 a series a day: a replay asks every window, and one
  rule at a millisecond made `serve` hold gigabytes); a query Grafana's own
  query fails on, at every evaluation: a range over Loki's 11,000 points (a
  step of its own of 1s over thirty days), a step Grafana can't read (a
  number, `1.5`, months), a `queryType` it doesn't run; classic conditions
  over Loki; a condition reading Prometheus and Loki together; and a
  condition reading any data source that is neither PromQL nor LogQL (SQL
  ones among them), by the type Grafana gives.
- **Measured** against Loki 3.7.9, Grafana 13.3.1 and Prometheus 3.5.0:
  over forty minutes, 88 of 88 firings of a Loki ruler's nine rules alike,
  and 166 of 166 of Grafana's seventeen on Loki, each starting and ending at
  the same evaluation, where v0.26.0 had 23% and 20%. Kept as test data
  (`TestRealLoki`, `TestRealLokiGrafana`), with a real Loki's answers at
  six millisecond steps and what Grafana 13.3.1 asked Loki for, and run
  against real servers by `test/e2e/loki.sh` in CI. The chart takes
  `--loki URL --loki-loaded-rules` (and `--loki-tenant`) through
  `extraArgs`, beside its rules or alone; a token file or rule files to
  mount need values of their own, still to do.
- **Spanish and Japanese** for the UI and for what `serve` posts
  (`--lang es`, `--lang ja`, the chart's `serve.notify.lang`), offered in the
  language picker. Written with a machine's help, in the words SREs use;
  they want a native speaker's review.
- **Provenance beside each release**, from v0.28.0 on: the attestations'
  Sigstore bundles, for the archives, `SHA256SUMS`, the chart and the
  image, as `lapilli-vX.Y.Z.intoto.jsonl`, a release's seventh asset;
  SECURITY.md says how to check a download with them, offline, with `gh`
  or cosign, held to the release's tag, as checked against v0.27.0's.

### Changed

- **An accessibility review of the UI, to WCAG 2.2 AA**
  (docs/design/access.md): a skip link; each page's title its own; the
  navigation and the language picker labelled, each language named in its
  own; a name for every field, comment box and link; header cells with
  scope; every chart read out, its numbers in a table (the precision chart's
  weren't), and checked against it; links and field borders that can be
  told apart at 3:1; one visible focus ring; no sideways scroll at 320px;
  in-page jumps clear of the header. axe-core over every page in four
  languages, light and dark, 0.27's on-call schedules and tracker comments
  among them, went from 136 failing elements to none (`test/a11y/axe.sh`);
  `TestPagesAccessible` (in more than one state of a page),
  `TestContrast`, `TestChartsInTables` and `TestCSSAccessible` hold it. No
  script added.
- The content check makes no PromQL guess of a Loki ruler's rule (a range
  shorter than two scrapes, a label an aggregation can't give).
- Before, a Grafana rule whose condition read Loki was asked of
  `--prometheus` as PromQL: it failed, or, where LogQL also parses as
  PromQL, was replayed as never firing, or as NoData.
- A time an API writes is read exactly, not as a float64: a point at a
  step in milliseconds is no longer up to 128ns off it.
- A range refused for being too large is halved on its own grid, so a group
  placed within its interval keeps its evaluations. A range refused for its
  series (Loki's `max_query_series`, Mimir's
  `max_fetched_series_per_query`, both counted over the range) is halved
  too, down to a single evaluation.

### Fixed

- `go test -race ./internal/serve` passes: the tracker recipes' test fakes
  shared their state with the tests without a lock (70 race reports, and
  one more in 0.27's comment tests).

## [0.27.0] - 2026-11-11

What comes in (docs/design/inputs.md): every delivery Alertmanager made,
on-call schedules' turns, when a change was deployed, a cloud alarm's
replay checked against its history, Compass's closes, two more trackers
and comments kept in step. **Ledger schema 015**, after
0.25's 014: a table of on-call turns, and `comments` made again with three
columns (`origin`, `ref`, `author`) and ids never used again
(AUTOINCREMENT), its rows and ids as they were; no flag changes meaning.

- **Alertmanager's event recorder** (0.33.0 on, `--enable-feature=event-recorder`):
  `POST /v1/alertmanager/events` on the notifications port, with the
  notifications token, takes its webhook output, one event or a batch.
  Each notification event, a delivery by an integration, is kept as a
  notification to `RECEIVER/INTEGRATION` (`oncall/pagerduty`,
  `oncall/slack`), so PagerDuty's, Slack's and email's deliveries are
  counted as the webhook's are; both formats are read (0.33 and 0.34's
  label pairs, 0.35's maps), as recorded from real Alertmanagers 0.33.1,
  0.34.2 and 0.35.0. A delivery sent again is kept once; other events are
  answered and passed over, at a rate of their own; `?skip=NAME` passes
  over Lapilli's own webhook receiver kept beside it.
- **Shifts from on-call schedules**: `--on-call FILE`, for `verdict` and
  `serve`, names iCalendar feeds (PagerDuty, Opsgenie, Jira Service
  Management, Grafana OnCall and IRM, Google Calendar; by a URL in a file,
  a URL and a token, or a file) and the people in them with their hours;
  `--shifts on-call=NAME`, or a team's shifts, counts each firing by the
  hours of whoever's turn it came in. `serve` reads each feed every
  `every` (30m) and keeps the turns in the ledger past what the feed
  holds; its runs page shows each schedule. Daily and weekly repeats,
  `EXDATE` and `RECURRENCE-ID` are read; what isn't is passed over and
  said. The chart's `serve.onCall` mounts the file and its Secret.
- **When a change was deployed**: `recipes/versions/argocd.sh` and
  `flux.sh` write the revisions an Argo CD Application or a Flux
  Kustomization or HelmRelease deployed and when, and with `DEPLOYS`,
  `git.sh` writes the rules each deploy put in place, by when it did,
  rather than each commit by its time.
- **A cloud's PromQL alarm's replay, checked**: `lapilli serve` sets a
  CloudWatch, Google Cloud or Azure PromQL alarm read with `--alarms`
  beside its history copied in under its source, tied by `alarm_id`, as it
  sets rules beside `ALERTS`; a CloudWatch alarm's replay is put together
  as the alarm as a whole first. `lapilli verdict --check-replay
  --check-firings FILE` does the same from a recipe's output. Without the
  history, such an alarm says why it isn't compared.
- **Compass's closes**: `recipes/incidents/compass.sh` reads each closed
  alert's log (`/v1/alerts/{id}/logs`) and ends it at its last close
  (`compass-closed.jq`), at most `MAX_LOGS` (500) alerts a run, rather
  than at its last update.
- **Trackers**: `recipes/trackers/azure-boards.sh` and `shortcut.sh`, alike
  with the others, and in the chart (`serve.trackers.tracker`). With
  `COMMENTS=true` (the chart's `serve.trackers.comments`), every tracker's
  recipe keeps comments in step both ways, each once: people's comments on
  a task are posted to its issue, quoted, and the issue's copied to the
  task as the tracker's. The tasks token gains `GET` and `POST
  /api/v1/tasks/{id}/comments`, and sends 64 KB at most a request (was 16).
- **Fixed before release, after two reviews** (docs/design/inputs.md,
  "What the reviews found"): Alertmanager 0.35's muted alerts aren't
  counted as delivered; a notification the ledger won't keep doesn't drop
  the rest of a batch, which is answered with a count of what was refused;
  a storm of other events doesn't refuse deliveries; a tracker comment
  Lapilli refuses is passed over rather than failing every run, one with no
  text or too long is made to fit, and an emoji or a soft hyphen is kept;
  Lapilli keeps the tracker's id of each comment it posted (the tasks
  token's `PATCH /api/v1/tasks/{id}/comments/{comment}`), so none is posted
  twice; restricted comments aren't copied, and a comment is posted without
  who wrote it unless `COMMENT_NAMES=true`; a comment on a closed issue
  doesn't close a task reopened in Lapilli; Azure Boards' Basic process; on
  call, people added later are found in turns kept, repeats are bounded
  and cheap, `WKST` is honoured, a removed turn at a feed's head goes, and
  events far outside the span read don't take kept turns away; deploys are
  walked as deployed (rollbacks, reverts, failed reconciles, the deploy in
  effect at `SINCE`, look-alike tags); a day held for a cloud's alarms'
  history is told of when checked.
- **Fixed before release, after a verification** (docs/design/inputs.md,
  "What the verification found"): a comment written after a posted one was
  deleted is posted, not taken for it (ids never used again); a tracker's
  comment whose text or author's name doesn't show is kept as what does
  (U+FFFD, "someone"), under its id, so such comments no longer starve the
  rest, and the job stops at the 1,000 a task keeps and counts only what
  was kept; a ref another comment has is a 409, not a 500; Shortcut's
  archived story is dated by its history, so the job's own comment doesn't
  drop a reopened task again, and a Jira without
  `statuscategorychangedate` is read by `resolutiondate`;
  `lapilli_serve_notifications_refused_total{source}` counts notifications
  refused; on call, `who` and the summary come before a lone attendee the
  file doesn't know (PagerDuty's), names are cut to the ledger's 200
  bytes, an event passed over keeps its turns kept, the ledger keeps a
  feed's 50,000 turns, a person in the file twice is refused, and two at
  once is said; deploys: a revision is found among the clone's objects (a
  tag or a branch named like it doesn't hide it), only deployed releases
  count, `MAX` counts the deploys that changed the rules, a date-only
  `SINCE` is the start of that day in UTC, and a kept file without a final
  newline is read whole.
- **With 0.25's roles and names**: a comment's author from a tracker and an
  on-call turn's name follow the ledger's rule for names (`Hidden`); a
  comment copied in is the tracker job's (`tasks-token`, a name the sign-in
  proxy can't give), so no one, an admin or a team's editor, changes it;
  team scope is people's, and the tasks token copies and marks on every
  team's tasks.
- **Reviewed as an attacker would**: the events path reads no body before
  the token, refuses what isn't the recorder's, and bounds events and
  alerts; feeds are fetched over https only, with no redirect followed, 8
  MB and 30 seconds at most, their URLs never shown, and their events,
  lines and repeats bounded; deploy histories reach `git` only as hex and
  RFC 3339; Compass's and the trackers' next pages are rebuilt from
  checked cursors; comments from a tracker are kept as text that shows,
  10,000 characters at most, 1,000 a task, never told, and can't be edited
  in Lapilli. Each refusal has a test.

## [0.26.0] - 2026-11-11

Replay fidelity (docs/design/states.md): what Grafana and Prometheus do
around a rule's condition, replayed where history can say it, and said
where it can't. Set beside a real Grafana 13.3.1 and Prometheus 3.5.0,
the replay of the same rules went from 56% of firings alike to 100% for
Grafana's, each starting and ending at the same evaluation, and from 82%
to 100% for Prometheus's read from its API; the recording is kept as test
data and replayed by every test run. Reviewed before release; what the
review found is fixed in what is described here, and measured again with
its shapes added (a series there 60s of 120s with `for` and with
`keepFiringFor`, `keepFiringFor` through no data and while its value is 0,
a pending alert whose series goes and comes back): over 46 minutes, 255 of
255 firings alike, where v0.22.2 had 37% and this branch before the review
62%. Verified again on gauges Prometheus scraped every 15s, with a data
source whose own scrape interval is 30s and three rules changed while
Grafana ran; what that found is fixed too (docs/design/states.md, "What
the verification found"): over 40 minutes, 135 of 135 firings alike,
where before the verification 35% were.

### Added

- **Grafana's states, as its state manager keeps them**: Normal, Pending,
  Alerting, Recovering and NoData, moving only on an evaluation's results,
  with the rule's `for` and `keep_firing_for` worked out there. An
  evaluation at which a query returned nothing, or the condition came to
  nothing, becomes what the rule's `noDataState` says: an alert of its own
  in the NoData state (labelled `grafana_alertstate="nodata"`, as
  Grafana's `GRAFANA_ALERTS` has it), every alert held as alerting, let go,
  or kept as it was. An alert whose series has gone stays as it was, a
  pending one never firing while gone, for
  `missing_series_evals_to_resolve` evaluations, then resolves with no
  `keep_firing_for`. A condition's NaN is Alerting, as in Grafana.
- **No data told from no history.** A query is taken to have returned
  nothing only once it has returned something, in the replay or the day
  before, looked through as often as the rule is evaluated, a minute at
  most: before a server's retention, or before a metric existed, nothing is
  NoData. A rule whose query has returned nothing for over a day is
  replayed as quiet. What the day before said is asked once for every set
  of rules replayed together, and kept, so `serve`'s next run doesn't ask
  again while it is within the day; a question that fails fails the rule,
  as a query does.
- **Only the data source a Grafana rule's condition reads** is asked, its
  queries outside the condition too, for whether they returned anything:
  Grafana takes the rule as having no data when one of them returns
  nothing, or is refused as written (as Grafana 13.3.1 was seen to). A
  query on another data source (a Loki query whose values an annotation
  shows) isn't asked, nor an expression over it; and one outside the
  condition that fails otherwise doesn't fail the rule.
- **A Grafana rule's alerts carried across an edit Grafana carries them
  across**: what it says, `keepFiringFor`,
  `missing_series_evals_to_resolve`, its group's interval. They start
  afresh only on what Grafana's fingerprint of the rule holds: its uid,
  title, folder, group, labels, condition, queries, pause, notification
  settings, `for`, dashboard and panel, and its no data and error states.
- **Classic conditions with every reducer**: `diff`, `diff_abs`,
  `percent_diff`, `percent_diff_abs` and `count_non_null` too, and the
  `gte`, `lte`, `eq`, `ne`, `no_value` and `_included` range evaluators and
  `logic-or`, evaluated as Grafana's own code does, with its no data, null
  and NaN.
- **Near misses by value** for a Grafana math expression joining
  comparisons with `&&` or `||`, a comparison beside anything else, and
  classic conditions: which comparison nearly held (`$A`, `avg(A)`), how
  close it came, and the number that would have caught the incident.
- **`--late DUR`** and `--late PATTERN=DUR`, for `verdict`, `serve` and
  what an agent compares over MCP: replay as if metrics arrived that late,
  each evaluation reading the history as it stood then; a proposal's
  backtest in `serve` too. A Prometheus group's `query_offset` is read, and
  a Grafana query's window that ends before now.
- **Groups placed where Prometheus places them**, for rules read from its
  rules API (`--loaded-rules`): evaluated at the second of the interval the
  group last ran at, not on the minute.
- **"Replayed in part"**: a rule on a cloud's own monitoring replayed
  without `--late`, a Grafana rule with a query on another data source than
  its condition's, and one whose query outside its condition failed in the
  replay are marked so in `verdict`'s output and JSON (`unseen`), and the
  first two on the rule's page in `serve`. `--check-replay` says, by rule,
  how many of Grafana's error alerts it left out of what really fired.

### Changed

- A Grafana rule that leaves out `noDataState` is replayed with Grafana's
  default, `NoData`: where its queries return nothing, having returned
  something, it now fires, as it did.
- Grafana's range queries are read at Grafana's own resolution, measured:
  `intervalMs`, 1s when not set (it was 15s), and its window with both
  ends, for the reduces that count points (`count`, `count_non_null`,
  `sum`). The rest are asked at the scrape interval, `--scrape-interval`,
  which now goes with a replay too, or else at the shortest interval the
  series they read are scraped at, as Prometheus's `/api/v1/status/config`
  and `/api/v1/targets` say for the range check, `verdict` and `serve`
  alike; where neither says, at Grafana's resolution too. The server's
  own resolution, a minute, stepped over a 15s spike Grafana's points saw.
  The step is cut to one that divides the rule's interval and the window
  too, so each evaluation is one of the subquery's: asked every 15s, on
  the epoch's grid, a firing on a spike started an evaluation after
  Grafana's.
  A window's newest number is the query's own value at the evaluation. A
  rule evaluated more often than once a minute read its newest value up to
  a minute old.
- What really fired, for Grafana's rules, leaves out its error alerts:
  `GRAFANA_ALERTS{alertstate="firing",grafana_alertstate!="error"}`; and
  reads an alert's alerting and recovering series as one firing.
- A Prometheus rule with a `query_offset` fires at its evaluation, and is
  set beside `ALERTS`, which Prometheus writes at the query's time, that
  much earlier. Its key carries the offset: it is a new version once, and a
  change proposed to it before the upgrade and not yet carried out is
  refused as made to a rule that has changed; propose it again.
- `lapilli serve` replays with this only what its runs replay from now on:
  a day it has settled keeps the firings it was replayed with before.

### Fixed

- **A Grafana rule's `$` read as `$$`.** Grafana's file provisioning takes
  `$$` for a `$` in a rule's title, labels, group, folder and other
  settings, and its provisioning export, which `--grafana` reads, writes
  every `$` there as `$$` (seen on Grafana 13.3.1: a label
  `'{{ $$labels.team }}'`, a title `Cost $$5`). Lapilli read them as
  written: such a rule went by a name Grafana's alerts don't have, its
  labels never agreed with what really fired, and a change proposed to it
  was refused as stale once read again. They are read as Grafana reads
  them now; no environment variable is put in, as Grafana would. A rule's
  annotations and queries, which Grafana takes as written and doesn't
  escape, are read as written, as before: what an alert says was checked
  right. Such a rule is a new version once, and a change proposed to it
  before the upgrade is refused as stale; propose it again. The content
  check routes it by its folder and receiver as Grafana's notification
  policies name them.
- **Grafana's `$__interval`, `$__rate_interval` and `$__range`** (and their
  `_ms`, `_s` and `${...}` forms) are put in a query as Grafana's Prometheus
  data source puts them, as measured on Grafana 13.3.1: the step; the
  larger of the query's `intervalMs` and a scrape interval together and
  four scrape intervals, the scrape interval the query's own, else the data
  source's (`jsonData.timeInterval`, read from Grafana with `--grafana`;
  `--scrape-interval`, or Grafana's 15s, for one that can't be read); and
  the window. A rule with one couldn't be replayed ("query failed").
- A rule in a Grafana provisioning file with `$$` in its folder, group, uid
  or title couldn't be changed in the file ("the change can't be written
  into this document"): it is found by what Grafana calls it, and the file
  keeps its own escaping.
- `--check-replay` counts Grafana's error alerts only of the rules checked.

## [0.25.0] - 2026-11-11

`lapilli serve` run by the people who use it
([docs/design/serveops.md](docs/design/serveops.md)): quiet windows every
month or by cron, and a team's own; the weekly digest set in the UI; roles
given in the UI, of every team or of one; the range check by each job's own
scrape interval; and Grafana's notification templates checked beside each
rule. Ledger schema 014: `grants`, and a quiet window's team and schedule;
nothing changes on upgrade until someone uses them.

- **Quiet windows every month or by a cron expression**, beside every day
  and every week: `every: month` with `on` a day (`15`), `last`, or a
  weekday of the month (`first sat`, `last fri`), at the start's time of
  day; `every: cron` with `on` five cron fields, hourly at most. The first
  repeat is the window's start and end, and must be one of the schedule's
  times (a refusal names the next); each repeat is shorter than the
  shortest time between two over the four years from the start, a leap
  year's cycle. A firing is looked up once: in the last repeat to start by
  it, or by the end of Until's day, whatever the gap. Daylight saving is
  the reports' (`internal/schedule`, which also gains the weekday of a
  month for reports: `monthly first sat@09:00`).
- **A team's quiet window** (`team_id`) excuses only the firings of rules
  the team owns, worked out in the ledger as the pages work it out; a
  change to who owns what judges every team's window again, and a team
  removed stops its windows, as it stops its reports. A window's page
  names the team of each firing it excused.
- **Who owns a rule** is decided by its name first: a note's team, or else
  the first team, by name, whose pattern matches it, and only then the
  first whose labels it has. Before, the first team by name whose pattern
  or labels matched did, so a team owning by labels could take a rule
  another team's pattern named. Where teams overlap so, upgrading moves
  such a rule to the team its pattern names, and with it everything done
  by team: the rule's page and lists filtered by team, the teams' tallies
  and overview, its team's digest and reports, whose hours count its
  firings asleep or off hours, the incidents its catches give a team, and
  which team's editors may change its notes, tasks and quiet windows.
- **Roles given in the UI** (`/roles`, from the Teams page, and
  `/api/v1/roles`), with `--admins`: an admin gives an editor's role, of
  every team or of one, to `user:NAME` or `group:NAME`. Never admin or
  approver (the table refuses any other role), never everyone, and a role
  the flags give can't be taken away. A team's editors change their team's
  tasks, quiet windows and reports (to the team's own destination), notes
  on rules it owns (keeping their team) and comments on what is its; the
  ledger checks each inside the write, before and after, so a row can't be
  moved between teams. Only admins see who holds a role; a role taken away
  stops at the next request; a team removed takes its editors with it;
  the runs page shows roles given and taken to admins alone. A team's
  editors make ten of its reports at most, and the last ten of the
  ledger's fifty are an admin's.
  `GET /api/v1/me` gives the teams the reader edits, and the pages offer
  each reader what they may.
- **The weekly digest set in the UI**: on the Reports page and with `GET`,
  `PUT` and `DELETE /api/v1/reports/digest`. The flags define it until an
  admin sets it; from then on the ledger does, `--digest` isn't used (the
  log says so at start), and it can go to any named destination, a
  `--report-dest` too. It stays weekly, and its suggestions are worked out
  at its time whether it is sent or not. Upgrading changes nothing: the
  weeks sent are kept as before, so no week is sent twice and none is lost;
  a week due while it is off is passed over; moving its day never sends an
  older week. "Back to the flags" gives it back.
- **Reports of a team** can be made by its editors and by editors, sent to
  the team's own destination (`team:NAME`); reports for everyone, or sent
  elsewhere, stay an admin's. A week of the digest sent isn't sent again
  to a destination added after it went; the digest is on the Reports page
  to set whenever a report can go anywhere.
- **The range check by each job's own scrape interval**: `serve` and
  `lapilli verdict --check-content` read each job's `scrape_interval` from
  `/api/v1/status/config` and the job label its targets end up with from
  `/api/v1/targets` (a ServiceMonitor's job is its service's name). A range
  over a selector naming its job counts with that job's interval (the
  longest a `job=~` can be); any other with the global, or the one every
  job has. The finding says whose: `rate[3m] < 2 × 2m (job "batch")`.
  Checked against a real Prometheus 3.5.0.
- **Grafana's notification templates**, with `--grafana`: its templates,
  contact points and notification policies, read once an hour through the
  provisioning API with the rules' token (a Viewer's reads them), 8 MB an
  answer, no redirects. A rule's notifications are written with the
  templates of its own contact point (`notification_settings`) or of those
  the policies route it to by its labels, `alertname` and
  `grafana_folder`; a matcher on a label the rule doesn't set is followed
  both ways, and a template counts only when every way uses it. Three new
  checks: `notify_no_text` (an annotation a template shows the rule
  hasn't; fact), `notify_label_never_seen` (measured) and
  `notify_label_not_given` (heuristic, by a rule's one query). Policies'
  `match` and `match_re` are read; a rule's label written as a template,
  and policies past the 2,000 or 32 levels read, route both ways or not at
  all; `{{ or .x "fallback" }}` is guarded. Contact points are numbered
  and a way's are a bitset; routing is charged by the work done, a
  million a rule and twenty million a check, rules alike in the labels the
  policies match on are routed once, and the check routes for five
  seconds at most: the 1,697-policy tree a real Grafana accepted gives
  each rule up in about 20 ms, and 500 rules take 0.3 s. A rule given up
  on says so on its page, and why, and the log says how many, once a
  check. Templates are parsed, never run. Checked against a real Grafana
  13.3.1.
- **Korean**: quiet windows are 메인터넌스 윈도우, as DevOps teams say it.
- **Security (from before this batch):** a name the sign-in proxy gave
  with a character that doesn't show (a zero-width space, a soft hyphen, a
  word joiner) had it taken out, so it could become another person's, an
  admin's. Such a name, or one over 200 bytes, is no one's now: it may
  only look. Refused are control characters; format characters, but the
  zero-width joiner and non-joiner between two letters of one script or
  two emoji, as Persian names and emoji use them; characters that show
  nothing on their own (the combining grapheme joiner, the Hangul
  fillers); and a variation selector but after a symbol. A grant's name
  is held to the same, 200 bytes.
- **The weekly digest's outbox guard** read a key TellOnce didn't write,
  so from the second week every round worked the whole digest out again
  before finding it told (also in 0.23 and 0.24). It reads what TellOnce
  keeps, or the week 0.24 marked as passed over, when later.
- Reviewed three times, once as an attacker would and twice for
  correctness and cost, then the fixes verified and what that found fixed
  too: no way to an admin's or an approver's role was found; what was found
  is in docs/design/serveops.md, "What the reviews found". Every refusal
  has a test, and each fix and key check, taken out, fails one. A ledger
  from 0.24 migrates to schema 014 with the same stones.

## [0.24.0] - 2026-11-11

Changing a cloud's own alarms, step 3 of changing rules with a person's
approval (docs/design/alarms.md): Amazon CloudWatch's alarms, Azure
Monitor's alert rules, Google Cloud Monitoring's alerting policies and OCI
Monitoring's alarms, read through each cloud's API with `--alarms` and
changed through it, once a person approves, only those `--changes-in`
names. The four clouds gain the same; none of the four paths has run
against an account, only against stand-ins answering as each cloud's
documents show. No ledger schema change; but a CloudWatch alarm whose
actions are off now reads as off, so a ledger reading such an alarm from a
file gains a version on upgrade.

- **`--alarms SOURCE=URL`**, for `lapilli verdict` and `lapilli serve`,
  repeatable, beside `--rules`, `--loaded-rules`, `--grafana` or `--ruler`,
  or alone: a cloud's alarms through its API, the URL's host saying which
  (`https://monitoring.REGION.amazonaws.com`, Azure Resource Manager's
  subscription or resource group, a Google Cloud project's
  `alertPolicies`, OCI's `telemetry.REGION` with a `compartmentId`), read
  with that cloud's credentials whatever `--auth` says. One document an
  alarm, its state left out, so a version is a change to an alarm, not to
  its state; a read that fails anywhere keeps the version as it was. An
  alarm and a rule of one name are told apart by the source
  (`HighCPU [cloudwatch-prod]`). `lapilli serve` reads them every
  `--alarms-every` (10m), so the clouds' request quotas hold.
- **Read too**: an Azure metric alert in PromQL (one static criterion) is
  replayed, from files too; OCI's alarms say what they say (`GetAlarm` has
  the body the list hasn't); a CloudWatch alarm with `ActionsEnabled: false`
  reads as off.
- **An alarm and its history, one rule.** An alarm Lapilli doesn't replay,
  read through `--alarms`, and its history copied in under its source are
  one rule, judged under the alarm's name: tied by the alarm's own ID,
  which each firings recipe now labels its alerts with (`alarm_id`: the
  ARN, the resource id, the policy's name, the OCID), or by name where none
  is given, and at the severity the alarm has now where it changed (one
  that fired at the other within twelve weeks has no record). Google's
  policies are compared by the policy, their project named by ID or by
  number. Its record is twelve weeks its history was copied in for
  without a gap, up to now: each write's period is kept (in the ledger's
  meta table; no schema change), so copies that stopped for a while, or
  stopped longer ago than `--source-quiet`, are no record until the
  weeks missed are copied in. A ledger from before keeps its source's
  last period alone; a job copying weekly wants `--source-quiet 8d`.
  Without one, it has no record: it isn't among the rules that never
  fired, and retiring it isn't proposed.
- **The alarm path.** A proposal for such an alarm is applied through its
  cloud's API when `--changes-in` names it as `SOURCE:NAME` (CloudWatch's
  alarm name, Azure's `RG/NAME`, Google's display name, OCI's display name;
  a prefix ends in `*`) and no tag says another tool keeps it
  (CloudFormation's own, Config Connector's and Terraform's labels, or one
  `--changes-managed` names); otherwise it is for a person, and its page
  says why. A `for` and a threshold where the alarm is PromQL, backtested;
  what it says on all four, a runbook its own form; turning it off. What
  is written is the least each API takes: CloudWatch's whole alarm as read
  (`PutMetricAlarm`), its actions alone off and on, Azure's properties
  changed (`PATCH`), Google's fields in `updateMask`, OCI's fields with
  `if-match`. The rest of the alarm changed since, the change is made
  afresh; the rule itself changed, it is refused as stale. Read back, once
  more a round later where the cloud may be a moment behind.
- **Not every alarm**: CloudWatch's whose actions scale or act on an
  instance aren't changed; one that warms up isn't rewritten; a composite
  alarm is turned off and on alone; one with a field this Lapilli's model
  doesn't have isn't rewritten, nor deleted from here (its undo couldn't
  make it again whole). A Google policy of several conditions takes a
  condition's `for` and threshold alone.
- **Retiring**: an alarm Lapilli can't replay is turned off and no
  further, never deleted from here: the page says to delete it in the
  console once it has been off a while. A PromQL alarm is turned off, then
  deleted four weeks later, its replay checked.
- **Turning off and deleting wait while the rule fires**, on every path:
  the git path, Grafana, the rulers and the alarms alike, while the ledger
  has an alert of it still on (an alarm's by its ID too), or the cloud
  says so, read just before the write: CloudWatch's `ALARM`; an Azure
  alert of the rule whose monitor condition is `Fired` and that no one
  closed (one of a stateless rule, an activity log alert's or one that
  doesn't resolve itself, only within its evaluation window); an open
  Google alert of the policy; OCI's `FIRING`. A cloud that throttles or
  fails a while holds the change a round, uncounted. It says it stopped
  once it is carried out.
- **One API path with the rulers'**: an alarm is changed by the engine
  v0.23's rulers are, with each of their guarantees: one change to an
  alarm at a time, read back only where it was sent and again for two
  minutes, an undo put back only over the change as it left it and
  checked as any write. A CloudWatch alarm is written only in its own
  account (the role's, or the pod's own as STS says), and a Google policy
  deleted and made again by a person isn't made a second time by an undo;
  made again twice alike after a lost answer, it is closed as settled in
  the console. One rule whose API won't answer leaves the week's other
  proposals made.
- **Write identities**: `--changes-aws-role` and `--changes-azure-client-id`
  for alarms too; `--changes-google-service-account`, a service account
  impersonated through IAM Credentials; `--changes-oci-config`, an OCI
  config file and its API key. Each needs an `--alarms` of its cloud.
- **The chart**: `rules.alarms`, `rules.alarmsEvery`, and
  `serve.changes.googleServiceAccount` and `ociConfigSecret`, the key
  mounted only with changes on. Its README gives each cloud's least
  privilege.
- The weekly fuzzing reads the four clouds' alarm answers too.
- Reviewed as an attacker would and for correctness; what the reviews
  found is fixed: a notification ties to its alarm's firing by `alarm_id`;
  an undo is refused where the change it undoes would be; a composite
  alarm's parents are asked as AWS takes it; the waits above; ties by
  severity and across a policy's conditions; a record up to now; an Azure
  description added and undone; the chart's OCI URL with `compartmentId`
  anywhere in its query. A verification of those fixes found nine more,
  fixed too: Azure's stateless alerts, a record across two severities and
  across a gap in the copies, Google's two forms of a policy's name, an
  alarm with a field this Lapilli doesn't know deleted, a cloud that
  throttles, a failing lookup stopping the week's proposals, and the two
  policies alike. Each is held by a test that fails without it.

## [0.23.0] - 2026-11-11

Changing rules through a managed Prometheus ruler, step 2 of changing rules
with a person's approval (docs/design/rulers.md): Mimir and Cortex, Amazon
Managed Service for Prometheus and Azure's Prometheus rule groups, read
with `--ruler` and changed through their own APIs, once a person approves,
only in the namespaces `--changes-in` names. Mimir's path runs against a
real Mimir 3.2.4 in CI; Cortex's, Amazon's and Azure's against stand-ins
answering as their documents show, not an account. No ledger schema
change: a rule read otherwise keeps its key, so no ledger sees a new
version of its rules on upgrade.

- **`--ruler URL`**, for `lapilli verdict` and `lapilli serve`: the alert
  rules a ruler holds, as written, from the API that writes them. Mimir's
  and Cortex's configuration API (`--ruler-token-file`, and
  `--ruler-tenant` for one with tenants, sent as `X-Scope-OrgID`), Amazon's
  control plane (`https://aps.REGION.amazonaws.com/workspaces/ID`, with
  `--auth aws`), or Azure Resource Manager's rule groups in a subscription
  or a resource group (with `--auth azure`). A rule knows its namespace;
  rules of one name in two namespaces are told apart by both
  (`ScrapeSlow [payments/shop]`).
- **The ruler path.** A proposal for a rule read with `--ruler` is applied
  through the ruler's API when `--changes-in` names its namespace (Azure:
  `RG/NAME`; a prefix ends in `*`) and no tag says another tool keeps it
  (CloudFormation's own, or one `--changes-managed KEY=VALUE` names);
  otherwise it is for a person, and its page says why. What one write
  replaces (a group, an Amazon namespace, an Azure rule group) is read
  again just before: the rule itself changed since is refused as stale;
  other rules in it changed, the change is made afresh. It is kept for
  undo, written with its kind's write credential
  (`--changes-ruler-token-file`, `--changes-aws-role`,
  `--changes-azure-client-id`), and read back as rules: an Amazon namespace
  stays applying while it is `UPDATING`, and a unit that reads as neither
  it was nor as proposed fails. A group goes with its last rule; an Amazon
  namespace's or an Azure rule group's last rule is for a person.
- **Undo** puts back the rule as it was kept in the group as it is now,
  others' changes since left as they are.
- **Retiring on Azure** turns the rule off (its own `enabled: false`) and
  deletes it four weeks later, approved again, as Grafana's.
- **Every request** goes to `--ruler`'s host only, follows no redirect
  (nor an Azure `nextLink` elsewhere), and reads at most 64 MB for a list
  and 4 MB for one unit, within 30 seconds; names read from a ruler go into
  a path as one escaped segment.
- **The chart**: `rules.ruler` (`url`, `tenant`, `tokenSecret`), and
  `serve.changes.in`, `managed`, `rulerTokenSecret`, `awsRole` and
  `azureClientId`, the write credential mounted only with changes on. Its
  README gives the IAM policy and the Azure role.
- Google Cloud's `Rules`, `ClusterRules` and `GlobalRules` stay on the git
  path: a test now holds that a new `for` and a new threshold are written
  into one, the rest of the file as it was.
- `test/e2e/changes.sh` starts Mimir 3.2.4, which its Prometheus writes to,
  and runs the ruler path against it. The weekly fuzzing reads the three
  rulers' answers too.
- Reviewed before release as an attacker would and for correctness; what
  the reviews found is fixed: undo puts back one rule, only over the rule
  as the change left it, every key of it, never a group's other rules
  with it; a unit is read back by its rules' whole entries; one change to
  a unit at a time, read back only where it was sent; names counted by
  namespace and group together and escaped, so two can't read alike, and
  none with a `..` segment written to, nor a rule's own name taken for one
  Lapilli makes; one group Mimir writes in a form YAML can't read back
  (`|4`), or one Amazon namespace whose file doesn't read, is passed over
  and logged, not the whole read, and a Mimir namespace of more than 128
  bytes is read; an Azure rule group's id is kept in one letter case; a
  Mimir read back as it was is read again for two minutes.

## [0.22.2] - 2026-11-11

### Fixed

- **A rule Lapilli doesn't replay was proposed for retirement.** A cloud
  alarm that isn't PromQL, a Grafana rule on Loki: its firings, copied in,
  are kept under a name of their own, so after twelve weeks the rule read
  as never fired. With `--changes`, Lapilli proposed deleting it, its
  record empty, and the rules page listed it as never fired. Such a rule
  is neither now; one turned off still is.
- **One rule with a proposal waiting stopped the week's proposals.**
  Lapilli's own proposals stopped at the first rule a person had a
  proposal open for, and the rest weren't made that week. That rule is
  passed over now.

## [0.22.1] - 2026-11-11

Changes to rules carried out against real servers, not only stand-ins:
Prometheus 3.5, Gitea 28.3, Grafana 13.3 and GitLab CE 19.5. Two of the
paths v0.22.0 shipped didn't work against them; both are fixed, and
`test/e2e/changes.sh` now runs the git and Grafana paths against real
servers in CI.

### Fixed

- **Grafana: every change was refused as stale.** A rule was read again
  before it was changed from its own export, which Grafana 13 gives without
  its folder, so its group read differently from the export of every rule
  it was proposed against. It is read from that export now.
- **GitLab: `recipes/trackers/gitlab.sh` stopped at the first task.**
  GitLab 18 and later link an issue as a work item (`-/work_items/N`), which
  the recipe didn't take as its own, after making the issue. Both links are
  taken. Quick actions in a task's text (`/assign`, `/confidential`), which
  GitLab 19.5 was seen to carry out on an issue made through its API, stay
  escaped, as v0.22.0 made them.
- **Grafana: turning a rule off takes an Admin service account.** Grafana
  13 answers a pause from an Editor as taken and leaves the rule running.
  Lapilli found it when it read the rule back, and the change failed; now
  it also says why. The help, the design and the chart say which role is
  needed: an Editor's token changes a `for` or what a rule says.
- A Grafana rule's deletion proposed before the rule has been off four weeks
  is refused then, not once approved.

## [0.22.0] - 2026-11-11

Lapilli reads by default, and now changes only what a person approves: the
first principle in GOVERNANCE.md, rewritten with its design (#40). Changes
are off until turned on. With them: reports people schedule themselves, what
an alert says checked, and tasks kept in step with a tracker. The ledger
gains schemas 012 (reports) and 013 (proposals and approvals). Reviewed
before release as an attacker would and for correctness, twice, then
checked again once merged; what the reviews found is fixed in what is
described here.

- **Changing rules, with a person's approval** (docs/design/changes.md,
  step 1). With `--changes`, Lapilli proposes changes to rules from what it
  finds: a longer `for` for a noisy rule, a threshold or `for` for a near
  miss, and retiring a spare rule or one that never fires (off first, then
  deleted no sooner than four weeks later, approved again). People propose
  too, and agents over MCP behind `--mcp-propose`. Each proposal comes with
  its backtest over twelve weeks, on `/proposals` and the rule's page, and
  can be made into a task. A person approves it in the UI only, never
  through the API, MCP or a token, and never their own (`--approvers`;
  `--approvals 2` for two). An approved change is read and replayed again,
  then opened as a pull request by `recipes/changes/` (GitHub, GitLab,
  Bitbucket, Gitea or Forgejo, Azure DevOps), which hold the repository's
  credentials and take changes with `--changes-token-file`, or applied
  through Grafana's provisioning API (`--changes-grafana-token-file`), with
  the rule as it was kept for undo. Limits (`--changes-limits`, 5 deletions
  a day and 20 changes an hour by default) and a stop (`--changes-paused`, a
  file, or an admin's switch, looked at again just before each write) apply.
  One proposal waits for a rule at a time, and a person, with their agents,
  makes twenty an hour at most. A change sent to Grafana is kept as
  `applying`, with the rule as it was and counted against the limits, until
  it is read back. A Grafana rule is deleted only after its disable, and
  only while it is still off. The change recipes take only their own pull
  requests (same repository, into `BASE`, by the token's own account; Gitea
  tokens need `read:user`, and `GITHUB_LOGIN` names a GitHub App), and
  write only files under `RULES_PATHS`, never one a CI system runs, so a
  Lapilli someone took over can't push a workflow that would run with the
  repository's secrets.
  A pull request changed in review is `amended` (told as `change.amended`)
  rather than holding up its rule, and an admin can close one stuck opened,
  merged or applying, with a reason. Raising `--approvals` holds proposals
  approved by fewer people. `--changes` needs roles (`--editors` or
  `--admins`) and someone to approve (`--approvers` or `--admins`). The
  recipes take the pull request already open from their branch when the host
  refuses another; a proposal's task shows its pull request but keeps its
  own link. The chart adds
  `serve.changes`; its pause is a ConfigMap named `FULLNAME-changes-paused`,
  and `helm install` prints the command that makes it.
  A fix to what a rule says is proposed the same way: one annotation
  (`summary`, `description`, `runbook_url` or `runbook`) set to text a
  person or an agent wrote, or taken out, offered on the rule's page beside
  each content-check finding text can fix and carrying that finding as its
  evidence; it is written as one quoted line, the rest of the file byte for
  byte, and must parse as a template in the rule's language, call only that
  language's own functions (for Prometheus, those every version since 2.45
  has) and no `query`, and can't grow without bound when the alert fires
  (no `range`, no templates, no variables set; an escape or a replacement
  only on a value as it is). Lapilli never writes the text itself.
- **Reports people schedule themselves** (docs/design/reports.md). A Reports
  page and `/api/v1/reports` take reports on a preset (`daily@09:00`,
  `weekly mon@09:00`, `monthly 1@09:00` or `last`) or a cron expression,
  hourly at most, in the ledger's zone or the team's, over the calendar span
  before it or a span given (`24h`, `7d`). A report is for everyone or one
  team and holds the sections picked: summary, noise, missed, woken,
  notifications, replay, sources, tasks, content and, for a week or longer,
  suggestions. It goes by name, never a URL, to `--report-dest NAME=FILE`
  (the chart's `serve.reports.destinations`), `slack`, `team:NAME`, or
  `webhook` as `report.ready`. It is sent once for each span through the
  outbox, when the ledger has the span, or after it fell due by six hours or
  half the time to the one before, whichever is less; after a restart, only
  the last one due, if it isn't later than that time again. A time daylight
  saving skips falls due when the clocks jump, and a repeated time the first
  time. A report whose team is removed stays stopped. Admins write reports;
  everyone sees them and a preview. The weekly digest is unchanged and shown
  with them.
- **What an alert says, checked** (docs/design/content.md). Lapilli now
  reads what each rule says when it fires: summaries, descriptions, runbook
  links, templates and severities, from every source's own fields. Azure
  Monitor's alert rules and OCI's alarms are now read as rules too, listed
  but not replayed. It checks them. Facts: no summary, no runbook, a
  template that doesn't parse, no severity or one not in `--severities`, the
  same condition as another rule. Measured: a template label no firing had,
  a noisy rule with no `for`. Guesses, kept apart, each with why: a label an
  aggregation can't give, a rare severity, a range shorter than two scrapes,
  `absent()` with no `for`. Templates are parsed, never run; links are
  checked, never visited. Each rule's page has "What it says", the rules
  list marks problems of fact, `/metrics` has
  `lapilli_rule_content_problems{rule, check}`, and MCP carries it all as
  data; a report's `content` section gives the problems now, by check. The
  check takes time in proportion to an expression's length, is worked out
  again only when the rules or firings change, and leaves out a rule it
  can't read rather than failing. `lapilli verdict --check-content` does the
  same without a ledger. `--check-content=false` turns it off.
- **Tasks kept in step with a tracker** (docs/design/trackers.md).
  `--tasks-token-file` lets a job, on the `--incidents-listen` port, read
  tasks (with who set each one's status, `status_by` and `status_at`, and
  `updated_after=`) and set their link and status, and nothing else. The
  link must be https, at most 2,048 characters, and is set once. The token
  has its own rate and turn and is refused if another token's file holds the
  same value.
  Its writes are recorded as `tasks-token`, don't count as anyone's use, and
  aren't sent to the notice webhook. `recipes/trackers` has scripts for
  GitHub, GitLab, Jira and Linear. Each run makes an issue for every open
  task without one and links it. A task follows its issue when the issue is
  closed, and reopens if the job had closed it. With `CLOSE_ISSUES=true`, an
  issue is closed when a person closes its task. A recipe keeps in step only
  the issues its own account made, labelled `lapilli` where labels are by
  name: an issue filed ahead with a task's marker, or one linked by hand, is
  neither followed nor closed. An issue is a task's only by the recipe's own
  last marker line, which the task's text can't forge; Jira team labels are
  `lapilli-team-NAME`. A GitHub App or Actions token names its account with
  `GITHUB_LOGIN`. A task the tracker refuses is skipped and the run fails at
  the end; a deleted issue is reported and its task left as it is. The
  chart's `serve.trackers` runs the script as a CronJob. Task links typed in
  the UI can now be 2,048 characters long.

### Security

- **`go 1.26.11`.** Go's `net/http` could crash an HTTP/1 server whose
  handler reads a request's body while writing its headers, past a
  `MaxBytesReader` (GO-2026-6859, CVE-2026-97054, published on the day
  v0.21.0 was). Lapilli's handlers read a body before they answer, so no
  way to reach it was found, but v0.21.0's archives and image were built
  with Go 1.27.3 and carry the flaw. This release's are built with Go
  1.27.4, and `go install` now needs 1.26.11 at least.

### Fixed

- `lapilli serve --read-only=false` and `--notify-slack=false` were taken as
  `--read-only` and `--notify-slack`: the value after `=` was ignored. They
  now read it, as `--check-content=false` does.

## [0.21.0] - 2026-11-11

The replay set beside what really fired, as it goes; rule versions from
before the ledger began; a cloud's own alarms' notifications, every cloud
alike; and four more incident recipes. The first release reviewed as an
attacker would before it shipped, as CONTRIBUTING.md now asks.

- **The replay beside what really fired, kept** (docs/design/fidelity.md).
  `lapilli serve` sets each day the runs won't replay again beside
  `ALERTS` and `GRAFANA_ALERTS` (or `--check-series`), each firing counted
  on the day it started, and keeps it (schema 010). A rule's page says, over
  twelve weeks, how many were alike, how many only in the replay and how
  many only in what really fired, with the last of those; the rules list
  and the dashboard mark a rule whose replay differed over the last week
  (`--check-differs N/RATIO`, 5/0.9 by default), and `replay.differs` tells
  the webhook and Slack, once a week while it does. The runs page says how
  far it has got, which series held nothing, and what really fired with no
  rule by that name. `lapilli_rule_replay_check{rule, way}` in `/metrics`,
  in MCP's `rules` and `rule`, and a line in the weekly digest. No stone
  changes. On with rules to replay; `--check-replay=false`, or the chart's
  `serve.checkReplay.enabled: false`, turns it off.
- **Rule versions from before the ledger began** (docs/design/history.md).
  `--rules-history DIR`, for `serve` and `verdict`, reads earlier versions
  of the rules, one directory each, named by when it took effect
  (`2026-10-14T16:40:31Z` or `20261014T164031Z`). `verdict` reads it as each
  version given as `--rules DIR@TIME`. `serve` adds them to a ledger that
  has no version yet, so its first backfill replays each day with that
  day's rules rather than all of it with today's; a ledger with versions of
  its own keeps them, and a version that can't be read stops the start with
  the ledger as it was. `recipes/versions/git.sh` writes the directory from
  a clone: the rules as they stood at `SINCE`, then each commit on the
  branch that changed them, a merge at the merge's time, with `RENDER` for
  rules rendered from a chart. The chart's `serve.rulesHistory` runs it in
  an init container on each start.
- **A cloud's own alarms' notifications, every cloud alike**
  (docs/design/notifications.md). CloudWatch alarms' messages to SNS,
  directly or through EventBridge, are taken on `/v1/sns`, told from Amazon
  Managed Service for Prometheus's by their shape, a contributor's
  attributes among an alarm's labels. Azure Monitor's other alerts are
  taken on `/v1/azure`, their resource now the alert's target, lower-cased
  here and in `recipes/firings/azure-monitor.jq`. Google Cloud's other
  policies are taken on `/v1/google`. OCI Notifications is taken on a new
  `/v1/oci/SOURCE`, with the token as the password, checked before the body
  is read; a subscription is confirmed only from such a request, only over
  https to a Notifications API host, each URL once; the path waits two
  seconds for its turn, then answers 503. `?source=NAME` (the path for OCI)
  ties an alert to the rule copied in from that source by the name the
  alarm sent, though the ledger names the rule apart; an OCI alert, by its
  alarm's OCID, which `recipes/firings/oci.jq` now gives its firings as
  `alarm_id`. A message of an alarm resolving or repeating takes its start
  from the last notification of its group. Schema 011.
- **What takes input from outside is reviewed as an attacker would**
  before a release, as CONTRIBUTING.md now says. This release's reviews
  found, and it fixes before shipping: a notification whose alerts don't
  say their start is read with one bounded query (it held the ledger's
  writer for up to a minute with 1,000 alerts against a busy rule), from
  notifications of the same kind and labels in the last month. An OCI
  alert's alarm id names its rule when read, so a message that comes before
  its firing is copied in still ties. EventBridge's other CloudWatch events
  are kept as nothing. Azure resources are compared without case, so
  v0.20's rows still tie. The SNS certificate and OCI confirmation fetches
  reach only internet addresses, and only Oracle's own realm domains. A
  history version that links outside itself is refused, one dated in the
  future is skipped, and an empty directory, or a restart that can't reach
  git, starts with today's rules; `git.sh` keeps versions in the branch's
  order, refuses links, and runs `RENDER` without the token. `verdict`'s
  `--rules` beside `--rules-history` is the newest version. Rootly's recipe
  waits out a 429, Rootly's and FireHydrant's de-duplicate across pages,
  and Compass's stops before 20,000. The replay check counts rules that
  share an alert name together, starts from the first whole day the runs
  replayed, pairs firings across midnight alike from either day, and keeps
  a day a day where clocks skip midnight (the ledger's midnight is now a
  day's first instant there). A review of those fixes found more, fixed
  too: a history version that is itself a link is checked where it leads;
  the chart's `RENDER` runs in an init container without the token
  mounted; a commit dated after now stops `git.sh` rather than hide every
  later version; a link elsewhere in the repository no longer stops
  `RENDER`; rules that share an alert name only some days are summed day
  by day; a start is found however long ago its alarm went off; the fetches
  refuse a few more ranges that aren't the internet (NAT64, 6to4,
  benchmarking, reserved, Azure's host address) and keep no connection
  open.
- **Four more incident recipes**: Grafana IRM, Rootly, FireHydrant, and
  Compass's alerts, for Opsgenie's users as its support ends on 5 April 2027
  (its incidents move to Jira Service Management, which `jira.sh` reads).
  Each checks `SINCE` before asking, bounds its paging, and writes nothing
  when the tool fails; tested against each API's published schema with a
  server standing in, not against real accounts.

## [0.20.1] - 2026-11-10

A security release for the two ports 0.20.0 opened, after an audit from an
attacker's point of view: the notifications port, which may face the
internet, and the firings a token copies in. Nothing let anyone past a
token or a signature; what was found let them take the port down, drop
SNS's messages, or make a write cost far more than it should.

- **The notifications port read a body before its proof (high).** Anyone
  who could reach it could send many bodies of 1 MB at once, unproven, and
  run lapilli out of memory. A path a token proves is now refused before
  its body is read, and only 64 bodies signed by SNS or Grafana are read
  at once, each within ten seconds, and only when its headers say it could
  be: SNS's message type and a topic taken, or Grafana's signature. Past
  that the port answers 503, which SNS sends again.
- **SNS's messages could be made to fail (medium).** A certificate URL
  spelled many ways filled the certificate cache, and fetching spent a
  budget all shared, so a genuine message got 403, which SNS drops for
  good. A certificate is now taken only from the SNS host of its topic's
  region, spelled as SNS spells it; one that has verified a message stays
  kept; one that can't be fetched now gets 503; a URL with no certificate
  behind it doesn't use up the fetches. A message signed more than
  an hour ago isn't taken, and the same message, or the same Grafana
  signature, sent again is answered as kept without using a turn. Each
  source has its own rate, and a topic not taken and a bad signature get
  the same answer.
- **A firing with a far end judged the whole ledger again (medium).** One
  short write could hold the ledger's writer for seconds, each time. A
  write now judges again no further than a period past its period, nothing
  in it may start or end more than an hour ahead of lapilli's clock, and
  the firings token writes twenty periods at once, then one each five
  seconds.
- **A write's cost grew with the square of the rules copied in (medium).**
  Finding a source's rule didn't use its index, and each new name read
  every version of the rules. Sent again unchanged with 52,000 rules, 2,000
  rules took 8 s; now the index is used and the names are read once a
  write.
- **What a token could add had no bound (medium).** The ledger keeps 5,000
  rules from each source and 20,000 from all, 500,000 alerts from each, and
  200,000 alerts a day from each source of notifications. A rule's name is
  512 characters at most (CloudWatch's are 255, Google Cloud's 512), a
  receiver's 200, an alert's labels 64 and 4 KB; anything else is refused
  with 400. A group key past 1,000 characters is kept as its hash. `/metrics` counts
  notifications only for the ledger's rules, and each rule's ten busiest
  receivers, so a flood of names can't flood the Prometheus that scrapes
  it.
- **`go 1.26.10` (medium).** `go install` with Go 1.26.0 to 1.26.9 built
  lapilli with 37 known flaws in the standard library, among them
  `net/http`'s and `html/template`'s. The release archives and the image
  were built with Go 1.27.3 and weren't affected.
- **Low.** Names with a character that doesn't show (a line break, a
  zero-width space, a mark that turns text right to left) are refused, so
  a rule copied in can't look like another or forge a line of the digest.
  A rule copied in no longer takes a name a note, task or comment waits
  under, which a replayed rule coming later would have lost to it. A
  source is as far as the furthest period it has written, so writing 1970
  each hour can't hold the digest back. The notifications token may not
  be the incidents or firings token, at start and on each request. Slack's
  one-line notices are posted as written, with no formatting or link
  previews. MCP tells agents that receivers, sources and the labels of
  firings copied in and of notifications are data too. `put.sh` prints
  what the server says as JSON. A period sent past now counts as written
  up to now. The firings token's 429 says to try again in five seconds. A
  body cut off is answered 408, not 413.
- The AWS SDK's updates (#34). SQLite stays at 3.53.4 for now: 3.54.1
  wrote a week of 100,000 firings a fifth slower, past the budget on
  macOS.

## [0.20.0] - 2026-11-10

Firings recorded elsewhere, kept in the ledger and judged with the rest, past
what the cloud keeps; notifications as they were sent, tied to the firings
they told of; and both told to Slack and the webhook.

- **Firings recorded elsewhere, kept in the ledger** (docs/design/recorded.md).
  With `--firings-token-file`, the port `--incidents-listen` opens also takes
  `PUT /api/v1/firings/SOURCE?from=&to=`: a source's firings that start in
  the period, one a line as `--firings` reads them, in place of those the
  ledger had from it then. Sent again as they are, nothing changes or is
  judged again. The token is its own: one that copies incidents can't write
  firings, and the reverse. They are judged as replayed firings are, and
  their rules are named as their sources name them, told apart from another
  rule with the name (`checkout-5xx [cloudwatch-prod]`); a replayed rule
  coming later takes the name. A recorded rule's page says where it was
  recorded, and that what needs its condition isn't there. Runs leave
  recorded firings alone, `--keep` drops them and forgets a source not
  written since, and a write is 31 days in whole seconds, 16 MB and 100,000
  firings at most, with 50 sources. Schema 008: `rules.source` and `rules.sent`, and
  `sources`.
- **`lapilli serve` on firings copied in alone.** With the firings token,
  `--rules` and `--prometheus` are optional: nothing is replayed, and what
  comes in is judged as it comes. `--impact` still needs `--prometheus`.
- **Told to Slack and the webhook.** A source nothing has been copied in
  from for `--source-quiet` (default 26h; the chart's
  `serve.firings.quietAfter`) is told of once, as `source.quiet`, to the
  webhook and, with `--notify-slack`, to Slack. The weekly digest says which
  sources firings were copied in from, how many each brought and which has
  gone quiet, and what the notifications sent came to (how many, repeats,
  while people slept, for nothing, the rules that sent the most for
  nothing); a team's digest, for its rules. The webhook's `digest.ready`
  carries both as `sources` and `notifications`.
- `recipes/firings/put.sh` sends a recipe's output as a source's firings for
  `SINCE` to `UNTIL`. The runs page lists the sources, `/api/v1/sources`
  has them, and `/metrics` has `lapilli_serve_source_last_timestamp_seconds`
  and `lapilli_serve_source_firings` by source. The chart takes
  `serve.firings.tokenSecret`, with `rules` and `prometheus` optional then.
- **Notifications as they were sent.** `lapilli serve --notify-listen ADDR`
  opens a port that takes notifications, and nothing else, a path for each
  source: `/v1/alertmanager` (a webhook, with the token in
  `--notify-token-file` as its bearer token), `/v1/grafana` (a webhook
  contact point, with the token or its HMAC signature with the secret in
  `--notify-grafana-secret-file`), `/v1/sns` (Amazon SNS's signature, from a
  topic `--notify-sns` names, for Amazon Managed Service for Prometheus; the
  subscription is confirmed at an SNS host only), `/v1/azure` (an action
  group's webhook, common alert schema, the token as the URL's password) and
  `/v1/google` (a Cloud Monitoring webhook channel, basic auth after a 401).
  Each alert is tied to its rule's firing whose labels are all among its own,
  started within two evaluations; a notification the same as one from
  another replica within a minute is kept once; repeats are as the source
  says, or the same alerts again with nothing new and nothing resolved. Rule
  pages and the rules list show notifications, repeats, those while people
  slept and those for nothing, by receiver; firings, how many told of each,
  or that one paged no one; the dashboard's "Woken for nothing" counts
  notifications once there are any. `lapilli_rule_notifications{rule,
  receiver, reason}` in `/metrics`, and in MCP's `rules` and `rule`. Schema
  009. The chart's `serve.notificationsAPI`.

## [0.19.0] - 2026-11-09

A cloud's own alarms: replayed where they are written in PromQL, on every
cloud that has such alarms, and judged from their history where they aren't.

- **CloudWatch PromQL alarms.** `--rules` reads the AWS CLI's
  `describe-alarms` answer: a PromQL alarm is replayed with its pending
  period as `for`, its recovery period as `keep_firing_for` and its
  evaluation interval; metric and composite alarms are listed as not
  replayed. `--prometheus https://monitoring.REGION.amazonaws.com --auth aws`
  queries CloudWatch's PromQL API, signed for the service `monitoring`, as
  is its dual-stack (`REGION.api.aws`) or VPC endpoint name; `aps` stays for
  Amazon Managed Service for Prometheus. A period given as NaN is refused.
- **Google Cloud alerting policies with a PromQL condition.** `--rules`
  reads them as `gcloud monitoring policies list` writes them, in YAML or
  JSON, or as the API's list answers: the condition's `duration` is `for`,
  its evaluation interval (30 s when it doesn't say) the rule's, its
  `alertRule` and `ruleGroup` the name and group (the policy's display name
  otherwise), its labels the rule's, and the policy's severity becomes
  `severity`. An alert closes at the first evaluation at least the larger of
  270 s and two evaluations after its condition last held, as Google Cloud
  closes them. A disabled policy, and one with another kind of condition, is
  listed as not replayed. Replay with `--gcp-project`, whose PromQL API
  answers for the same metrics, and `--step 30s`, of which every policy's
  interval is a multiple.
- **An answer cut short is an error.** An API that caps the series in an
  answer and says so only in a warning (CloudWatch: 500) no longer gives a
  replay of what was left: the rule's query fails, saying why.
- **Firings from a cloud's alarm history.** `recipes/firings` has a script
  for each cloud that writes its alarms' history as `--firings` takes it:
  `cloudwatch.sh` (`describe-alarm-history`), `azure-monitor.sh` (Azure
  Resource Graph), `google-cloud.sh` (`projects.alerts.list`) and `oci.sh`
  (`get-alarm-history`), each with the cloud's own CLI, tested against the
  clouds' documented answers with stand-ins for the CLIs. Times are checked
  to be times before they reach a query or a filter (UTC for Azure), the
  token goes only to https, a CloudWatch move that says neither its old nor
  its new state stops the script, and a stateless Azure alert (activity log,
  service and resource health) ends as it fires.

## [0.18.0] - 2026-11-09

For an incident nothing caught, the rules that nearly caught it by one of
their several numbers; and hours that take turns, for teams on call in
shifts.

- **Nearly caught, by one of several numbers.** A Grafana range threshold
  (`within_range`, `outside_range` and their `_included` forms) has each end
  looked at, the other as it is; PromQL joining comparisons with `and`, `or`
  or `unless` has each comparison with a number looked at, the others as
  they are, four at most. The first comparison of an `and` or `unless` is
  read by its values, as a single comparison is; any other by replaying the
  rule with nearer round numbers, the furthest within a fifth first, then
  searching back towards the rule's own for the nearest that fires during
  the incident. A comparison on the right of `unless` kept the rule quiet,
  and its number moves the other way. An expression over 16 KiB, or a
  comparison nested more than 32 levels (brackets, and, or, unless), isn't
  read for numbers. The near-miss table's `CHANGE`
  names the comparison (`rate(requests[5m]) > 10 → 8`, `unless requests < 10
  → 9`); JSON's `value` gains `of` and `unless`, and MCP the same.
- **Shifts.** For a team on call in shifts, or by turns across time zones,
  `--shifts` (the chart's `hours.shifts`), or a team's `shifts` in `lapilli
  serve` (the teams page and API; schema 007), gives hours that take turns:
  how long each turn is, in hours, days or weeks, when the first starts, and
  each turn's working hours, hours asleep and zone, those it leaves out as
  they are otherwise, `-` for a turn on them all (`every=1w
  from=2026-11-02T09:00; tz=Asia/Seoul; tz=Europe/Berlin sleep=23:00-07:00`).
  The first turn starts from 1970 to 2200. A firing is counted asleep or off
  hours by the turn it came in. A team's own hours or shifts replace the
  server's shifts. Reports name the shifts (`hours.shifts` in JSON).
- Working hours may run past midnight, as a night shift's do:
  `mon-fri@22:00-06:00` works the mornings after. They were refused.
- A near miss by value whose value wasn't read leaves out `reached`, in JSON
  and MCP; the table shows `–`.
- A firing a near miss's window made up at its start, the condition holding
  from before it, and what kept on from it through a dip by
  `keep_firing_for`, isn't taken for one that caught the incident: when it
  really started isn't known. Windows start further before the incident,
  and a near miss by `for` is checked by replaying the rule with the shorter
  `for`, as `keep_firing_for` keeps a firing on. Both were so in 0.17 too,
  with a group interval longer than the step, `keep_firing_for`, or a lead
  over a day.
- In `lapilli serve`, hours that change more than six times a day are counted
  with one read of the firings, row by row, rather than a query for each
  stretch.

## [0.17.0] - 2026-11-09

For an incident nothing caught, the rules whose value nearly reached their
threshold; near misses for incidents written down after the digest; and
working and sleeping hours for each team.

- **Nearly caught, by value.** For an incident nothing caught, a rule whose
  condition compares a value with a number (`errors > 0.9`, `0.9 < errors`, a
  Grafana threshold or a math expression with one number) nearly caught it
  when its value came within a fifth of the number for the rule's `for`. The
  report gives the round number short of what the value reached that would
  have caught it (`> 0.9 → 0.87`), when the rule would then have fired, and
  what it would cost over the period, as near misses by `for` do. A rule with
  no `for` is looked at this way too. In `lapilli verdict`'s report and
  `--json` (`value`), and in serve on the incident's page, in the digest and
  through MCP (`op`, `threshold`, `reached`, `new_threshold`).
- **Near misses for incidents written down later.** `lapilli serve` works out
  the near misses of a week again when its incidents change after the digest,
  or as they are written down in the week under way, within the hour, rather
  than at the next digest; and once more when the ledger's firings reach the
  end of an incident written down before it was over. Only the incidents that
  changed are looked at again; when the history fails, what was kept stays,
  and they are tried again later.
- **Hours for each team.** In `lapilli serve` a team can have its own working
  hours, hours asleep and time zone (the teams page, or `work_hours`,
  `sleep_hours` and `tz` in the teams API; each empty one is the server's).
  Its rules' firings are counted by them on the rules pages, the overview's
  tile, `/metrics`, `/api/v1/report`, MCP and its digest; a report says which
  teams went by their own hours (`hours.teams` in JSON). Schema 006 adds the
  columns.
- The near-miss table's `FOR` column is now `CHANGE`: `for 30m → 10m`, or
  `> 0.9 → 0.87`.
- The teams page's "Rules matching" column showed how many rules a team owns;
  it shows the pattern.
- A rule's `for`, `keep_firing_for` or group `interval` below nothing, or past
  a hundred years, is refused when the rules are read; from a rules API it is
  taken as nothing, or a hundred years.

## [0.16.0] - 2026-11-06

When the firings that caught nothing came, those that woke people first; and,
for an incident nothing caught, the rules that nearly did.

- **When the firings came.** The firings that caught nothing are counted by when
  they started: while people slept, off hours, or at work, in `--tz`.
  `--work-hours` (default `mon-fri@08:00-18:00`) and `--sleep-hours` (default
  `22:00-08:00`) say when those are, or `none` (in the chart, `hours.work` and
  `hours.sleep`). The report lists the rules that woke people for nothing first;
  `--json` has each rule's `asleep`, `asleep_black`, `off_hours` and
  `off_hours_black`; `--metrics` has `lapilli_rule_nothing_by_hours`. In
  `lapilli serve`, the dashboard has a tile for the firings that woke people for
  nothing, the rules pages and MCP have each rule's, and so does `/metrics`.
- **What nearly caught it.** For an incident nothing caught, or only noisy rules
  did, the rules whose condition held about then but not for their `for` are
  named, with the longest shorter `for` that would have caught it, when the rule
  would then have fired, and what that would have cost over the period:
  `lapilli verdict`'s report and `--json` (`near_misses`), and, worked out with
  `lapilli serve`'s weekly digest, the digest, the incident's page and MCP. A
  rule that held long enough, but fired only after the incident was over, is
  one too. The cost is of the rules as they stood, with quiet windows excusing
  what they excuse. It is bounded: twenty incidents, five rules each, ten
  replays tried, 2,000 questions, within the period; in `serve` the suggestions
  and these share one `--timeout`.
- A noisy rule's suggested longer `for` now counts, in `lapilli serve`, the
  firings a quiet window would excuse as the ledger does.
- `/api/v1/report?firings=false` over 31 days or less reads the firings again to
  count them by the hours, so it now takes its turn, one at a time, as a report
  with every firing does. Over longer periods it counts no hours.

## [0.15.0] - 2026-11-06

What each rule adds to the others, a check that the replay is right, incidents
copied in from where they are kept, and digests for each team and for a
webhook. Read "Before you upgrade": an incidents file with nothing in it is now
refused.

- **What each rule adds.** Each rule now gets the incidents it caught, those it
  caught before any other rule, and those no other rule caught, in the report,
  `--json`, `--metrics` (`lapilli_rule_incidents`,
  `lapilli_rule_incidents_first`, `lapilli_rule_incidents_alone`), the rules
  pages and MCP. A noisy rule's catch doesn't count beside one by a rule that
  isn't noisy. A rule is called spare when, for each of two incidents or more,
  another rule that can still fire caught it as soon, at the same severity or
  above; of rules tied, the first by name stays, so all the spare rules could go
  at once. The dashboard asks whether a spare rule still earns its place, after
  the incidents nothing caught, and offers a task to retire it. With `--rule` or
  `--changed-only`, only how many incidents each rule caught is given.
- **Is the replay right?** `lapilli verdict --check-replay` sets a replay beside
  what really fired, rule by rule, from `ALERTS`, `GRAFANA_ALERTS`, or the series
  `--check-series` names. It exits 1 when nothing was compared.
- **Incidents from where they are kept.** `recipes/incidents` copies them from
  PagerDuty, incident.io, Jira and GitHub issues. `lapilli serve
  --incidents-listen` and `--incidents-token-file` open a port that takes
  incidents from whoever holds the token, and does nothing else (in the chart,
  `serve.incidentsAPI`). What it copies in is the tool's (origin `copy`): the UI
  can't change it, and the token can't change what people wrote. It takes one
  write at a time, 20 a second, and keeps 20,000 copied incidents at most; they
  aren't told to the notice webhook or Slack, since the tool they come from has
  told people already.
- **An incident written again as it was changes nothing:** no change noted, no
  notice, no judging again.
- **The weekly digest goes further.** `--team-digest TEAM=FILE` posts a team its
  own rules and incidents (in the chart, `serve.teamDigests`), and
  `--notify-webhook-file` gets `digest.ready`, for everyone and for each team,
  with the numbers and the report as `text`. Webhooks that take Slack's format
  work; any other chat can take the JSON. Over twelve weeks, the digest names the
  rules that are spare. Its lines on tasks and outcomes now come right after the
  report's first lines, so a long report cut short for Slack keeps them.
  `digest.ready` has the report as written, `text`, and escaped as Slack takes
  it, `slack_text`.
- **A rule with `keep_firing_for`** is replayed from that much earlier too, so an
  alert held on across the start of a period is seen.
- The report's "never fired" no longer counts a rule that caught an incident
  with a firing from just before the period, or whose firings a quiet window
  excused.
- An incident's labels are 4 KB at most in all, as kept, and kept as written
  rather than with `<`, `>` and `&` escaped; its times are from 1970 to 9999. A
  change to an incident's title alone judges nothing again.
- An id in the incidents file that a person's incident holds is passed over and
  reported, on the status until the file reads cleanly and in
  `lapilli_serve_incidents_passed_over`; before, it kept the whole file from
  being read. One that a copy holds, the file takes.
- The README says plainly that Lapilli counts firings, not the notifications
  they become.

### Before you upgrade

- **An incidents file with no `incidents` key is refused**, by `lapilli verdict`
  and by `lapilli serve`, where the ledger then keeps the incidents it had. Before,
  such a file, an empty one among them, read as no incidents; and since a file
  owns what it holds, `serve` took every incident from it out of the ledger. A
  job that failed while writing the file leaves just that. For a period with no
  incidents, write `incidents: []`.

## [0.14.0] - 2026-11-05

A security release, after an audit from an attacker's point of view. Read
"Before you upgrade".

- **DNS rebinding (high).** A web page could point a name of its own at a
  local `lapilli serve`, or at a `kubectl port-forward`. The browser then
  took the server for the page's own site, past the cross-origin check. The
  page could read everything, change the settings, and write or delete
  incidents. `serve` now answers only these host names:
  - an address and `localhost`;
  - a name without dots, or under `.localhost`, `.internal`, `.svc` or
    `.cluster.local`;
  - `--external-url`'s name;
  - those in `--allowed-hosts` (in the chart, `serve.allowedHosts`).

  Others get 421; the health checks answer whatever the name. Over
  loopback (this machine, a port-forward, a sidecar), names without dots
  or under `.internal`, `.svc` or `.cluster.local` aren't answered either:
  someone with a hand in the company's or the cluster's DNS could point
  them here.
- **Forged headers inside a cluster (medium).** Any pod could reach `serve`
  and set the user or groups header, and so become an admin.
  - The chart now refuses `serve.userHeader` without
    `serve.networkPolicy`, unless `serve.networkPolicy.othersKeptOut` says
    something else keeps the rest out.
  - `/metrics` and the health checks are served alone on a second port:
    `--metrics-listen`, in the chart `serve.metricsPort` (9091). The probes
    and the ServiceMonitor use it, and `serve.networkPolicy.metricsFrom`
    lets Prometheus in there and nowhere else.
- **Running out of memory (medium).** A report with every firing of a busy
  month could take gigabytes, and several at once the pod. A report now
  gives 100,000 firings at most, excused or not, as do the settings tried
  over a week. Both take one turn at a time, held until the answer is
  written, with 30 seconds to write it, so a client that doesn't read can't
  hold answers in memory. The next waits 30 seconds, then gets 503.
- **A crash from a Grafana rule (medium).** A math expression naming a
  function without its parenthesis (`log`) dereferenced nil while the rules
  were read. A Grafana alert rule could crash `serve` reading Grafana, or
  `verdict --grafana`. Fuzzing found it.
- **Slack messages forged (low).** A task's assignee and a comment's key
  went into Slack lines unquoted, so newlines in them made a message of an
  editor's own under lapilli's name. Both are quoted now. An assignee is
  one line, and a comment must be on a rule, an incident or a task that is
  there.
- **A person taking a group's role (low).** In `--editors` and `--admins`,
  `user:NAME` is now a person alone and `group:NAME` a group alone. A name
  with neither is either, as before.
- **Hardening:**
  - Task and runbook links can't carry a user name.
  - The language cookie is Secure over HTTPS.
  - The MCP instructions name every field that people or the measured
    systems wrote, as data.
- **Fuzzing.** Every parser of outside input is fuzzed: rule files and
  APIs, scoping PromQL, Grafana expressions, and incident and firing files.
  CI runs each for two minutes every week.

### Before you upgrade

- **A host name to allow.** Behind a proxy or an ingress that passes a host
  name, give that name in `--external-url` or `--allowed-hosts`
  (`serve.externalURL` or `serve.allowedHosts`), or requests get 421.
- **A NetworkPolicy, in the chart.** `serve.userHeader` now needs
  `serve.networkPolicy.enabled` with `from` (the proxy) and `metricsFrom`
  (Prometheus), or `serve.networkPolicy.othersKeptOut: true`. Take
  Prometheus out of `from` if it was there: whoever is in `from` reaches
  the UI and can set the headers.
- **The metrics port.** The ServiceMonitor scrapes the `metrics` port.

## [0.13.0] - 2026-11-05

- `lapilli serve` speaks MCP at `/mcp` (Streamable HTTP, no sessions, one
  JSON object per answer), so an agent can ask it what the pages show and
  check a change to the rules before anyone makes it:
  `claude mcp add --transport http lapilli http://localhost:8080/mcp`.
  - **Tools that read:** `overview`, `rules`, `rule` (a rule's definition
    as in effect, its twelve weeks, versions, reviews, catches and latest
    firings), `incidents`, `firings` (with labels) and `outcomes`. Each
    answers as JSON with a schema.
  - **`compare`** takes the changed or new rules as a Prometheus rule
    file, and names of rules to take away. It replays only the rules that
    differ, as they are and as proposed, over up to 14 days, and judges
    both against the ledger's incidents. It returns each rule's firings,
    stones and precision before and after, and the incidents caught
    differently, with how soon. It is off unless `--mcp-compare editors`
    (or `admins`) turns it on, since it runs PromQL the agent wrote with
    Lapilli's access to Prometheus; in the chart, `serve.mcpCompare`. It
    needs roles: without them everyone is an admin. It
    takes 20 rules at most, `for`s of 48 hours at most, and no Grafana
    rules. One comparison runs at a time, within 25 seconds.
  - Nothing an agent asks for changes anything.
  - Requests with an `Origin` other than `--external-url`'s are refused,
    against pages elsewhere and DNS rebinding.
  - The server's instructions tell the agent what the stones mean, and to
    treat what people wrote as data.
- The official MCP Go SDK (v1.8.0) is a new dependency; the binary grows
  by about 2.5 MB.
- A Grafana rule whose expressions refer back to themselves is reported as
  not replayable. Before, replaying it recursed until the process ran out
  of stack (`lapilli verdict --grafana`, and `serve` reading Grafana).
- "Never fired in twelve weeks" now means twelve weeks on the rules page
  over 1 or 4 weeks too. Before, it was the weeks shown.

## [0.12.0] - 2026-11-05

- An outcomes page for `lapilli serve` (효용, in Korean): does Lapilli
  earn its keep? It answers from what the ledger already holds, sends
  nothing anywhere, and doesn't count page reads.
  - **Does it help?** Each reviewed change to a rule is set beside what
    came of it. The review's replay of the week before is the prediction.
    The four weeks either side are measured: firings a week, incidents
    caught, and firings fewer since than at the rule's rate before. It
    came out as predicted when its firings a week since are within a
    quarter, or two, of the prediction. A rule written counts its firings
    as more, and shows the incidents it caught since; a rule taken away,
    the firings it no longer makes; so a rule renamed comes to nothing. A
    rule paused, or that can no longer be replayed, ends the measure of
    the change before it.
    Beside them, the weeks before `lapilli serve` began and the latest:
    precision, firings and ● a week, incidents missed.
  - **Is the record kept up?** When the latest incident written down
    started, flagged after two weeks without one (every firing then reads
    as ●; impact windows nobody decided on don't count); impact windows to
    decide; failed runs; rules not replayed; incidents by week by where
    they came from.
  - **Is it used?** Edits through the UI and the API, and by how many
    people with `--user-header`; suggestions acted on (a task made, or the
    rule changed); tasks done and how long they took.
- The overview has an outcomes card, and the weekly digest a line of them.
- `/api/v1/outcomes` gives them as JSON. `/metrics` adds
  `lapilli_serve_firings_avoided`, `lapilli_serve_nothing_avoided`,
  `lapilli_serve_rule_changes{outcome}`,
  `lapilli_serve_incident_last_timestamp_seconds` and
  `lapilli_serve_edits`: an alert on the last can say when incidents stop
  reaching Lapilli.
- The ledger keeps when `lapilli serve` first ran on it, and indexes each
  rule's days (schema 5). A ledger opened by this release can't be opened
  by an older one.
- Charts of small counts are ticked one by one, where before 1 incident
  could be ticked 0, 0.2, 0.5, 0.8.

## [0.11.1] - 2026-11-04

- `lapilli serve` is set in Heebo, the typeface of Argo CD's UI, served from
  the binary (its Latin, 45 KB, under the SIL Open Font License 1.1, whose
  text is served beside it). Korean uses the system's font, as before. The
  serif headings are gone; code and labels use the system's monospace. The
  CSP allows fonts from the server itself and nowhere else.
- After an upgrade, browsers fetch the new stylesheet at once: the pages ask
  for `/static/...?v=` a hash of the files, kept for good, where before the
  old one could be used for up to a day.
- A page kept by a browser is no longer answered "not modified" by another
  `serve` process on the same ledger: the ledger's generation starts again
  with each process, so after a restart or an upgrade an old page could be
  shown; every ETag now carries the process too.

## [0.11.0] - 2026-11-04

- A calmer look for `lapilli serve`.
  - **Colours:** cream surfaces like river pebbles, and a muted olive for buttons, links and the current page. Caught firings are a quiet blue, firings that caught nothing a warm grey, and the heatmap of where the noise is runs from light to deep lavender.
  - **Headings:** page headings and the dashboard's numbers are set in a serif.
  - **Layout:** the header is a pill that stays at the top; cards and tiles have soft edges; task statuses are tinted pills.
  - **Dark mode:** in the same family.
  - **Checks:** chart colours were checked again for colour blindness and contrast in both modes, and all text is 4.5:1 or more. Incident statuses keep their fixed colours, each with its icon.
  - On a phone, the header stays in place and the tiles sit two to a row.

## [0.10.0] - 2026-11-03

- Quiet windows repeat every day or every week, until a day or for good:
  a nightly batch, a weekly release. Each repeat keeps its local time
  across a change to or from daylight saving.
- Notices of what happens: a task made, assigned, its status changed, or
  overdue; an incident written down; a quiet window kept; a comment. To a
  webhook, as JSON with a link to the page (`--notify-webhook-file`,
  `--external-url`), and to Slack, as a line in `--lang` (`--notify-slack`).
  They go through an outbox written with the change, a sender for each
  place, and are tried again, further apart, for about eight hours; each
  carries an `id`, the same each time it is sent. `/metrics` counts them,
  and how many are unsent.
- Roles: with `--editors` and `--admins` (people, or groups the proxy lists
  in `--groups-header`, or `*`), only editors write, only admins change the
  settings and the teams, and everyone else may only look. Pages offer only
  what the reader may do, and say who they are; `GET /api/v1/me` too.
- A task keeps a link to its issue in a tracker. `PATCH /api/v1/tasks/{id}`
  changes only the fields given, as the task is when written, so a tracker
  can put the link back without undoing a change made meanwhile; marking a
  task's status in the UI does the same.
- The weekly Slack digest says how the tasks stand: open, overdue, done in
  the week.
- The chart takes `serve.roles`, `serve.notify` and `serve.externalURL`,
  and can add a NetworkPolicy that lets in only the sign-in proxy and
  Prometheus (`serve.networkPolicy`).

## [0.9.0] - 2026-11-03

What people keep, beside what the runs find, made and changed in the UI
and the API, in the same ledger:

- Teams own rules by a name pattern or labels, and incidents by labels;
  the dashboard, the rules and the tasks can be shown for one team.
- A rule keeps a note: its team, whether to keep, tune or retire it, its
  runbook, a note.
- Quiet windows (a deploy, maintenance, a load test) excuse the firings in
  them that caught nothing: counted apart, neither white nor black, so
  expected noise doesn't weigh on precision. A firing that caught an
  incident still counts. Reports give each rule's `excused`.
- Tasks follow up on a rule or an incident, with a kind, team, assignee,
  due date and status; a suggestion on the dashboard becomes one with a
  press, and shows when one is open.
- Comments on rules, incidents and tasks; only their author changes them.
- Every write is a same-site POST or an API call, refused with
  `--read-only`, and noted with who made it.

## [0.8.0] - 2026-11-03

- `lapilli serve`'s UI speaks Korean as well as English: the browser's
  language, or the one picked at the top of the page. Each language is one
  file of translations, and a test renders every page in every language
  and fails on a text without one.
- Larger text throughout the UI, and in its charts; on a narrow screen,
  or in a narrow card, the charts' text is larger still and every other
  week's label is left out.

## [0.7.0] - 2026-11-03

- `lapilli serve` opens on a dashboard: last week's precision, firings,
  incidents missed, time to catch and noisy rules beside the week before;
  firings, precision and incidents by week; where the noise is, rule by
  week; and what to look at first. It reads the weekly tallies, so it takes
  about 10 ms over 200,000 firings.

## [0.6.0] - 2026-11-03

`lapilli serve`: keep judging, with a ledger, a web UI, an API and metrics.

- `lapilli serve` replays the rules every hour over the hours since its last
  run, and the hour before again for points that came late, and records the
  firings in a ledger (one SQLite file, through the pure-Go
  `modernc.org/sqlite`). It reads the rules every minute and records each
  change as a version, so each hour is replayed with the rules as they stood;
  a change is replayed over the week before it, as it was and as it is.
  Once a week it judges the week before, suggests longer `for`s, and posts
  to Slack. On first start it replays `--backfill` (28 days); it keeps
  `--keep` (400 days).
- The ledger keeps each firing's stones, given against every incident it
  has, and each rule's by day and week: twelve weeks read as fast as one,
  and a change gives stones again only near it. Over random weeks its
  verdict is `verdict.Judge`'s. With two million firings an hourly run
  takes 0.18 s, writing an incident 52 ms, reading twelve weeks 2 ms.
- A rule a run couldn't replay, a query failing or time running out, is
  replayed by later runs from where it was missed; a run that runs out of
  time is tried over half of what is left.
- A web UI, rendered on the server with no scripts: the stones by week, each
  rule's with its versions and change reviews, incidents (written down
  there, impact windows confirmed or dismissed, a black stone said to be
  real), the firings by day, the settings that only judge the ledger again
  (tried on the last week before saving), and the runs. An API gives
  `lapilli verdict --json`'s report for any period and takes incidents;
  `/metrics` gives the weekly metrics without a Pushgateway.
- It listens on localhost, has no sign-in of its own, records changes under
  the header a sign-in proxy sets (`--user-header`), refuses posts from
  other sites, and with `--read-only` takes no changes.
- The chart's `mode: serve` runs it with its ledger on a volume; a Grafana
  dashboard of the metrics ships with the chart.

## [0.5.1] - 2026-11-03

- Release binaries keep their symbol table, so `govulncheck -mode=binary`
  can tell which vulnerable functions one holds. Without it, v0.5.0's
  binaries are reported as using `golang.org/x/crypto/openpgp`
  (GO-2026-5932), which lapilli doesn't import; its code is not affected.
- A release is published only if govulncheck finds nothing in the code or
  in any of its binaries, and the latest release is checked again every
  day. SECURITY.md says how fixes ship and how to check the binary you have.
- Dependabot follows the Go modules and the image's base too, two weeks
  after a release, and security updates at once.
- The chart has a README, an icon, and the annotations Artifact Hub reads,
  among them the image it scans for each chart version.

## [0.5.0] - 2026-11-03

What to change, replays closer to the real thing, and rules read where they
live.

- What to change: each noisy rule is replayed again with a longer `for`, and
  the shortest that still catches every incident it caught while firing at
  most half as often is suggested, with how much later those incidents
  would be caught. Rules show their `severity` label in reports,
  comparisons (firings counted by severity before → after) and `--metrics`.
  `--noisy-firings` and `--noisy-precision` set when a rule counts as noisy.
- Rules that share a name in one set, in different groups or with
  different labels, are told apart (`HighErrorRate [severity=critical]`),
  alike in every set compared or replayed.
- `--compare --changed-only` replays only the rules that differ. A directory
  with no rules in it is an empty set when comparing.
- `keep_firing_for` holds a firing alert past its condition, a group's
  `interval` is when it is evaluated (when a whole number of `--step`s), and
  Grafana's classic conditions are replayed, written as one PromQL
  expression the way Grafana evaluates them.
- `--loaded-rules` replays the alert rules the API at `--prometheus` has
  loaded (Prometheus, Thanos, Mimir, vmalert, Amazon Managed Service for
  Prometheus), and `--grafana URL` those a Grafana holds, through its
  provisioning export API. The chart takes `rules.loaded` and
  `rules.grafana`.
- `--rules` reads each cloud's managed rule groups: an AWS rule groups
  namespace, Azure's prometheusRuleGroups (scoped to their `clusterName`),
  and Google Cloud's Rules (scoped to their namespace), ClusterRules and
  GlobalRules; `kubectl get -o yaml` lists; and the directories below a
  directory, or behind a link to one.
- The README has `--impact` expressions for Istio, Linkerd, Envoy,
  ingress-nginx, Traefik and OpenTelemetry HTTP server metrics.

## [0.4.0] - 2026-11-03

- `--compare FILE`: judge a change to the rules before it is merged. The
  rules in place and those proposed are replayed over the same period and
  judged against the same incidents; the report shows each rule that
  differs, its stones before → after, and each incident caught differently.
  Rules alike in both are replayed once. The README has a pull request
  check to copy.

- Columns line up when rule or incident names are in Chinese, Japanese or
  Korean, whose characters take two cells in a terminal.

- Oracle Cloud: it has no managed Prometheus, so on OKE lapilli reads the
  Prometheus you run there, and `--auth oci` says so. The README says which
  of a cloud's own alarms aren't read yet: CloudWatch, Azure Monitor, Google
  Cloud alerting and OCI Monitoring alike.

## [0.3.0] - 2026-11-02

- The managed Prometheus of every major cloud: `--auth aws` signs requests to
  Amazon Managed Service for Prometheus with SigV4, `--auth azure` sends a
  token for Azure Monitor managed service for Prometheus, and `--auth google`
  one for Google Cloud Managed Service for Prometheus, each from the
  credentials the machine or pod already has. `--gcp-project` stays, short
  for Google Cloud's URL and `--auth google`. The chart takes
  `prometheus.auth`, `prometheus.awsRegion` and `podLabels`, and documents
  the workload identity of EKS, AKS and GKE alike.

## [0.2.0] - 2026-11-02

Rules as they stood at each time, incidents by service, replays three times
faster, and a weekly run.

- `--rules FILE@TIME` replays the rules as they stood at each time, for a
  period in which they changed. Where a version takes over, alerts carry over
  as Prometheus and Grafana carry them over on a reload.
- A Grafana reduce over a window sees the data from before the replay starts,
  as Grafana's query would, instead of a window cut short at the start.
- A rule is listed as not replayed if any rule of that name couldn't be.
- An incident's label can list several values (`service: [a, b]`), any of
  which agrees for `--match`.
- `--impact` looks at each series on its own, and its incidents carry the
  series' labels: by service, an incident says which services were hit, so
  `--match` works for them too. An impact window must now last
  `--impact-for` in one series. In `--json`, incident labels are lists.
- Replays are faster and take less memory. Rules that need the same query
  over the same day share one request; a day's queries are asked for
  together; Grafana math pairs series by key rather than each with each; and
  `--parallel` (8) rules run at once, the busiest first. A replay is fetched
  and evaluated a day at a time, so a long period needs no more memory than a
  day.
- A Helm chart runs `lapilli verdict` every week as a CronJob
  (`oci://ghcr.io/lapilli-project/charts/lapilli`), and an image holds the
  release's Linux binaries (`ghcr.io/lapilli-project/lapilli`); both are
  attested like the archives.
- `--push URL` puts the metrics on a Pushgateway (job `lapilli`, or the group
  a URL ending `/metrics/job/...` names), and `--slack-webhook-file FILE`
  posts the report to a Slack incoming webhook whose URL is in the file; the
  URL is never shown, errors included, and redirects aren't followed.
- An incidents file with the same id twice is refused.
- `--rules DIR` reads every file with alert rules in a directory, such as a
  mounted ConfigMap.

## [0.1.0] - 2026-11-02

The first release of `lapilli verdict`, which judges alert rules against what
actually went wrong.

- Firings from replaying alert rules against a Prometheus-compatible API
  (`--rules`), from the `ALERTS` series (`--alerts`), or from a JSONL file
  (`--firings`).
- Rules from Prometheus rule files, `PrometheusRule` resources, Grafana
  alerting provisioning files, and ConfigMaps holding them. Grafana rules are
  evaluated the way Grafana does: reduce, threshold and math, with
  comparisons giving 1 or 0 and sets of series joined by labels.
- Incidents from a YAML file and from windows where an impact expression
  holds (`--impact`).
- A white stone for each firing that caught an incident, a black one for each
  that caught nothing; refired alerts; incidents no rule caught, and how long
  each took to catch.
- Catches by noisy rules (10+ firings, under 20% catching anything) are marked;
  incidents caught only by them are reported apart.
- Text, JSON (`--json`) and Prometheus metrics (`--metrics`).
- Busy APIs are asked again; answers too large are asked for in halves; a
  replay that runs out of `--timeout` still reports what finished.
