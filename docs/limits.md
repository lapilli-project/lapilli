# What it doesn't do yet

- Queries to data sources that are neither PromQL nor LogQL (SQL,
  Elasticsearch, InfluxDB, a cloud's own logs) can't be replayed. Read with
  `--grafana`, such a rule is judged from what Grafana's own alert state
  history says it fired, with no longer `for`, near misses or backtests,
  its labels as the history keeps them; read from files, or where the
  history can't be read or doesn't tell it apart, it is listed with the
  reason ([docs/design/grafanajudged.md](design/grafanajudged.md)). A Loki
  ruler's group runs a fraction of a
  second past a second, which the replay evaluates on: a log line written in
  that fraction is counted an evaluation later.
- Query errors aren't replayed: history can't say when a query failed.
  `--check-replay` says how many of Grafana's error alerts it left out of
  what really fired.
- A query that has returned nothing for over a day, or never in the
  history there is, is taken to have no history, not no data: such a
  Grafana rule is replayed as quiet, where Grafana may have sent NoData.
- A group without an `interval` is evaluated every `--step` (1m): the
  global `evaluation_interval` isn't in rule files, nor is the global
  `rule_query_offset`. An interval that isn't a whole number of steps is
  evaluated every step. Prometheus places a group within its interval by a
  hash of the path it loaded the file from, which a file doesn't say: read
  from a file, a firing can start up to an interval sooner or later than it
  did. `--loaded-rules` knows where.
- Grafana reduces a query's points as far apart as its `intervalMs` (1s by
  default) or more over a long window; lapilli asks for them so for
  `count`, `count_non_null` and `sum`, and for the rest at
  `--scrape-interval`, or else at the shortest interval the series they
  read are scraped at, as `/api/v1/status/config` and `/api/v1/targets`
  say, or, where neither is served (Mimir, Thanos, a managed Prometheus),
  at Grafana's 1s, which costs the server fifteen times and more the work:
  give `--scrape-interval` there. At the scrape interval, the step is cut
  to one that divides the rule's interval and the window too, so a sample
  is seen at the first evaluation after it, as Grafana sees it (a rule
  every 10s on series scraped every 15s is asked every 5s).
- A Grafana rule's query on another data source than its condition's (a
  Loki query whose values an annotation shows) isn't replayed: when it
  returns nothing, Grafana takes the rule as having no data, which the
  replay can't see, and the rule is marked "replayed in part".
- Replays see the history as it is now. `--late DUR` replays as if metrics
  arrived DUR after their time, but a delay that varies can't be read from
  history: a rule that resolved and fired again while the latest points
  were missing is replayed as one firing. Rules on a cloud's own monitoring
  are marked "replayed in part" until `--late` covers them.
- Rules alike in name, group and labels are counted together, and so are
  firings read with `--alerts` or `--firings`, which carry no group.
- A firing catches an incident by time and labels, or by the alerts the
  incident's tool linked to it, where a recipe copied them: PagerDuty's,
  incident.io's and Rootly's, and only those alerts that carry their
  `alertname` and labels. A linked alert's label the replayed firing
  doesn't carry is passed over, so one that told the firings of a rule
  apart only by it (two Prometheus replicas' external labels) links them
  both.
- `lapilli serve` runs as one replica on a ledger file, or as several on
  one Postgres database, one of them leading (docs/design/replicas.md).
  Who is who comes
  from its sign-in (an OpenID Connect provider, or a sign-in in front), and
  roles from the flags and the ones admins give; it doesn't sign people out
  at the provider, or refresh a session past `--session-max-age`. A version of the rules is in effect from when it saw
  it, up to `--watch` (1m) later, and for a mounted ConfigMap a minute or so
  more; the rules before its first start are taken to be those it found
  then, unless `--rules-history` says how they were. It gives a firing its
  stones against every incident it has, so a firing just before a week
  starts can catch an incident in that week, where
  `lapilli verdict` over that week alone sees only the week's firings.
- It counts firings, and the notifications they became only where the alert
  manager sends them to `--notify-listen`: Alertmanager's routing, grouping,
  inhibition and silences, and a Grafana notification policy, aren't read
  from their configuration. A firing that paged no one can't say why:
  silenced, inhibited and routed nowhere look the same from the receiving
  end. Notifications to PagerDuty, Slack and the like are seen only where the
  alert manager also sends them to Lapilli, or, from Alertmanager 0.33.0 on,
  its event recorder tells of them. The recorder's mutes say when a silence
  or an inhibition kept a firing from a notification, and are kept; a
  firing routed nowhere still can't say so, and 0.33 and 0.34 name the
  alerts a notification says were muted by a hash alone, so only their own
  mute events say which.
- Alarms kept in a cloud's own monitoring are replayed only where they are
  written in PromQL (CloudWatch PromQL alarms, Google Cloud policies with a
  PromQL condition, Azure's Prometheus rule groups). The rest are judged from
  their history, with `--firings` or copied into `lapilli serve`, and have
  no change reviews, longer `for`s or near misses.
- Datadog's monitors and New Relic's alert conditions aren't read as rules
  at all, in whatever language they are written: they are judged from their
  history only, as a cloud's alarms that aren't PromQL are, written by
  `recipes/firings/datadog.sh` and `newrelic.sh`. Both are checked against
  the answers each tool's documentation shows, not against real accounts.
- A CloudWatch PromQL alarm and a Google Cloud PromQL policy keep no series
  of what they fired: their replay is checked against their history only
  where it is copied in (or given with `--check-firings`), and CloudWatch's
  only as the alarm as a whole.
- Changes are carried out on four paths: a pull request where the rule is
  kept in git, Grafana's API, a managed Prometheus ruler's API (Mimir,
  Cortex, Amazon's, Azure's), and a cloud's own alarms' API (CloudWatch,
  Azure Monitor, Google Cloud Monitoring, OCI Monitoring), none of the
  clouds' run against an account yet. An alarm that isn't PromQL gets no
  `for` or threshold from Lapilli (nothing could backtest it), and is
  never deleted from here. Which tool made an alarm isn't asked of a
  cloud's deployment records: its tags and `--changes-in` decide. A
  Grafana rule's threshold is changed by a person, in Grafana. Google Cloud's `Rules` resources are
  Kubernetes resources, changed by the git path. Nothing on a Mimir's or a
  Cortex's group says which tool keeps it, and no ruler has a condition on a
  write: a write landing between Lapilli's read and its own is found when
  the change is read back, and fails.
- Pull request backtests are checked end to end against a real Gitea; the
  recipes for GitHub, GitLab, Bitbucket and Azure DevOps against stand-ins
  for their APIs, as they document them. A backtest's comment stops no
  merge by itself: a job stops one when it fails (`FAIL_MISSED`, an
  expectation broken) and sets a commit status (`STATUS`) a branch rule
  requires, GitHub's merge queue's (`merge_group`) on the merge group's
  head; never a GitHub check run ([pr-backtests.md](pr-backtests.md)).
- A pull request is opened only for a file in the repository that is the
  document Lapilli read. A rule file mounted from a ConfigMap whose manifest
  is what git holds, rules rendered from a chart, and rules read back from a
  cluster get a diff for a person instead.
