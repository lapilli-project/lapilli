# lapilli

Runs `lapilli verdict` as a CronJob, by default on Mondays at 09:00 over the
week before, or `lapilli serve` as a Deployment (below): it replays your
alert rules over the history in Prometheus and says which firings caught
something that went wrong (white stones) and which caught nothing (black
stones), which incidents had no alert, and which noisy rules a longer `for`
would quiet. The report goes to the job's log, and, if
you like, to Slack and a Pushgateway.

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' \
  --set impact='sum by (service) (rate(http_requests_total{code=~"5.."}[5m])) / sum by (service) (rate(http_requests_total[5m])) > 0.01' \
  --set 'match={service}' \
  --set report.slackWebhookSecret.name=lapilli-slack \
  --set report.pushgateway=http://pushgateway:9091
```

It needs:

- `prometheus.url`: a Prometheus-compatible API (Prometheus, Thanos, Mimir,
  VictoriaMetrics), or a cloud's managed Prometheus with `prometheus.auth`
  set to `aws`, `azure` or `google` and read access given through the
  cluster's workload identity. On Oracle Cloud, give the Prometheus you run
  on OKE, with no auth.
- the rules, from one of `rules.configMaps` (ConfigMaps in the release's
  namespace holding Prometheus rule files, PrometheusRule resources, Grafana
  provisioning files or a cloud's rule groups), `rules.loaded` (those the API
  has loaded), `rules.grafana.url` (those a Grafana holds) and
  `rules.ruler.url` (those a Mimir or Cortex ruler, Amazon Managed Service
  for Prometheus or Azure's Prometheus rule groups hold, as written); and,
  beside any of them or alone, a cloud's own alarms in `rules.alarms`
  (CloudWatch's, Azure Monitor's, Google Cloud Monitoring's, OCI
  Monitoring's, each named by the source its history is copied in under);
- what went wrong: `incidents`, `impact`, or both.

With `mode: serve` it runs `lapilli serve` instead: every hour into a ledger
on a PersistentVolumeClaim, or in Postgres (below), with a web UI, an API and `/metrics`, behind a
ClusterIP Service, or at a URL people open, with a sign-in (below). Incidents can then be written down in the UI, so
`incidents` and `impact` are optional; `rules.configMaps` takes one
ConfigMap, read every minute, each change becoming a new version.

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli -n monitoring \
  --set mode=serve --set prometheus.url=http://prometheus:9090 \
  --set 'rules.configMaps={alert-rules}' --set 'match={service}'
kubectl -n monitoring port-forward svc/lapilli 8080
```

The UI opens on a dashboard of the last twelve weeks, and keeps more than
the runs find:
- teams that own rules and incidents
- notes on rules
- quiet windows, once or repeating, that excuse expected noise
- tasks to follow the noisy rules and missed incidents up, with links to a
  tracker's issues
- comments

It speaks English, Spanish, Japanese and Korean (`serve.notify.lang` picks
what it posts in: `en`, `es`, `ja` or `ko`).

`serve.teamDigests` posts a team its own weekly digest. `serve.teamLabels`
names the labels whose values the Teams page suggests as teams (`team`,
`owner` and `squad` when empty). `serve.reports.destinations`
names the Slack-style webhooks, each in a Secret, that the reports people
schedule in the UI can go to; the UI picks among the names, never takes a URL. `serve.incidentsAPI`
opens a port that takes incidents, and does nothing else, from a job holding a
token: one that copies them from where they are kept. See `recipes/incidents`
in the repository. `serve.firings.tokenSecret` lets the same port take firings
recorded elsewhere, such as a cloud's own alarm history, with a token of its
own; see `recipes/firings`. With it, `rules` and `prometheus` are optional.
`serve.trackers.tokenSecret` lets it read tasks and set their link and status,
with a token of its own, and with `serve.trackers.tracker` (`github`, `gitlab`,
`jira` or `linear`), `image` (one with `sh`, `curl` and `jq`, by its digest,
`NAME@sha256:...`: a tag is refused) and `trackerTokenSecret`, a CronJob keeps
tasks in step with that tracker's issues every five minutes; see
`recipes/trackers`.
`serve.notificationsAPI` opens a port that takes notifications as the alert
manager sent them, and does nothing else:
Alertmanager's and Grafana's webhooks, Amazon SNS for Amazon Managed Service
for Prometheus and for CloudWatch alarms, Azure Monitor's action groups,
Google Cloud Monitoring's webhook channels and OCI Notifications'
subscriptions. Lapilli then counts each rule's notifications, repeats and
pages while people slept beside its firings, a cloud's own alarms' among
them.

`serve.rulesHistory` reads the rules' versions from the git repository they
are kept in, so a first start's backfill replays each day with that day's
rules. One init container clones the branch (`repository`, `branch`), with
every object, and a second runs `recipes/versions/git.sh` for `paths` since
`since` (by default the backfill and a day more; `max` versions at most),
into a volume `serve` reads with `--rules-history`. With `rules.configMaps`,
they are its history, and a ledger with versions of its own keeps them, so
this can stay on. With no `rules.configMaps`, the repository is the rules,
followed: two containers beside lapilli fetch the branch every `every`
seconds (300) and add the versions of its new commits
(`recipes/versions/follow.sh`); `serve` records each from its commit's
time, and replays the hours since then again. A commit is named no earlier
than when it was fetched, less `every` and a minute, by the clone's reflog,
which the fetch writes and the containers running `render` can't: a commit
dated back, by its committer's clock or because it waited in a branch,
takes effect when it reached the branch, not days before. Following never
stops on a commit: one that can't be rendered is passed over, one dated
after now is named now, and of more than `max` new commits the newest are
written. The versions from before the backfill and a day are taken away,
but the one in effect then. `serve.changes` is refused when the rules are a
followed history alone: there is no file of rules to write a change to.

For rules rendered from a chart, `render` is a command that writes them from
a commit's files: `helm template monitoring charts/monitoring`, or
`kustomize build overlays/prod`. Of what it writes, only the documents of
rules are kept (ConfigMaps of rule files or of Grafana provisioning files,
`PrometheusRule`s), and `select`, a label selector, keeps the labelled ones:
`grafana_alert=1` for the ConfigMaps Grafana's sidecar loads. Labels are on
Kubernetes objects only: a plain rule file has none, and `select` passes it
through. No `yq` is needed. `renderPrepare` runs before it in each commit's
files, for what a commit lacks, such as dependencies older commits don't
vendor (`helm repo add ... && helm dependency build charts/monitoring`).
`unrendered: skip`, the chart's default, passes over a commit that can't be
rendered or read, saying which and why, the version before staying in
effect; `fail` writes no history on a start (the recipe's own default).

With `render`, the versions are written in `renderImage`, which has git,
helm and kustomize: Argo CD's v3.5.6, pinned by digest, so a chart renders
as Argo CD v3.5.6 renders it. It is large, about 210 MB to pull and 870 MB
unpacked. To use a smaller one, or one with the helm and kustomize that
deploy your chart, set `renderImage` to an image with sh, git and what
`render` and `renderPrepare` run, by digest:
`renderImage: registry.example.com/tools/helm-git:1.2.3@sha256:...`.

A commit is read as it is: git runs with none of the image's configuration
(the Argo CD image turns Git LFS on), the commit's `.gitattributes` don't
leave a file out or rewrite one, and with `render` a commit holding a
symbolic link anywhere is passed over. A commit with more than
`maxTreeFiles` (20000) files or `maxTreeBytes` (256 MiB), as git counts
them before anything is written, is passed over too. The volumes have size
limits (`sizeLimits`: the clone 2Gi, what a commit is read into 1Gi, the
versions 1Gi) and the containers ephemeral-storage requests and limits
(`resources`).

`tokenSecret` names a Secret holding a token for an https repository that
needs one; only the containers that clone and fetch have it, and it goes to
the repository's host alone, so `render` never sees it. The clone is in a
volume of its own that only they write: those that run `render` have it
read-only, so nothing they run can change what the next fetch runs with.
The images are pinned by digest; every container runs as the pod does,
with a read-only root. When the repository can't be cloned, or the history
written, the directory is left empty and the log says so, rather than hold
the pod up: `serve` starts all the same, with today's rules; following,
with none, and the fetch clones the repository, the follow writes the
history, and `serve` takes it, its backfill replaying each day with that
day's rules, once they can. With `serve.networkPolicy.egress`, name
the repository's host in `to`, and the chart repositories `renderPrepare`
fetches from.

The rules history's containers run git, and `render`, on what the
repository holds, so a cloud's identity is kept from them. The service
account's token is never mounted in them: the pod doesn't mount it (`automountServiceAccountToken:
false`), and lapilli's own container has it, where Kubernetes would put it,
only with `prometheus.auth`, `gcpProject` or OCI's alarms, for a workload
identity that reads it (OKE's). EKS's webhook (IRSA and Pod Identity) and
Azure Workload Identity's are told to pass those containers over
(`eks.amazonaws.com/skip-containers`, `azure.workload.identity/skip-containers`,
added to `podAnnotations`). GKE's Workload Identity answers every container
in the pod from its metadata server, and no Kubernetes setting keeps one
container from it: on GKE, give `render` only commands that run nothing the
repository holds, or leave the rules history off.

`serve.checkReplay` sets the replay beside what really fired, a day at a
time once the runs won't replay it again, from `ALERTS` and `GRAFANA_ALERTS`
or the `series` given; a rule whose replay differs over a week (`differs`,
`5/0.9` unless given) says so on its page and in `/metrics`. It is on;
`enabled: false` turns it off.

`serve.ledger.postgres` keeps the ledger in a Postgres database, 15 or later,
rather than on a PersistentVolumeClaim (`docs/design/postgres.md`):
`secret.name` and `secret.key` name a Secret holding its connection string,
which goes in no argument, no log and no page. The whole Secret is mounted at
`/var/run/lapilli/ledger`, so it can hold the server's CA beside the string:

```sh
kubectl -n monitoring create secret generic lapilli-ledger \
  --from-literal=url='postgres://lapilli:PASSWORD@db.example:5432/lapilli?sslmode=verify-full&sslrootcert=/var/run/lapilli/ledger/ca.crt' \
  --from-file=ca.crt=ca.crt
helm upgrade lapilli ... --set serve.ledger.postgres.secret.name=lapilli-ledger
```

Say `sslmode=verify-full`: the server's certificate is checked, against the
system's CAs or `sslrootcert`'s. A string to a server on another machine that
says no `sslmode` (or `prefer`, pgx's default, which checks no certificate and
sends the password in clear to anything in the path that declines TLS) is
refused at start; `require` or `disable` is taken at its word, and serve warns. Give the database to the ledger's role alone (`REVOKE CONNECT ON
DATABASE lapilli FROM PUBLIC`): any role that can connect can take the
ledger's lock and keep serve from starting. No
PersistentVolumeClaim is made, and `serve.replicas` may be more than one
(below). The ledger's lock, and each write's turn, are a session's, so
connect directly or through a pooler in session mode, not transaction mode. With
`serve.networkPolicy.egress`, `serve.ledger.postgres.to` names where the
database is, as NetworkPolicy peers, and `port` its port (5432). A SQLite
ledger moves there once, with serve stopped: `lapilli ledger copy --ledger
DIR --to-url-file FILE`, from a pod that mounts both.

### Several replicas

```sh
helm upgrade lapilli ... --set serve.ledger.postgres.secret.name=lapilli-ledger --set serve.replicas=3
```

On a Postgres ledger, `serve.replicas` pods share it
(`docs/design/replicas.md` in the repository): one leads, on a lease the
database keeps, and runs what must run once (the runs, the jobs, the
digest and reports, the outbox, changes, the rules watched); every pod
serves the UI, the API, MCP, sign-in and the ports. When the leader's pod
is killed, another leads within some 17 seconds; when it stops, within 2.

- **Postgres only.** `serve.replicas` above 1 is refused without
  `serve.ledger.postgres.secret`: a SQLite ledger is one pod's file.
- **One at a time**: `RollingUpdate` (`maxSurge: 1`, `maxUnavailable: 0`),
  each new pod a follower until the leader it replaces gives the lease up;
  a PodDisruptionBudget lets `serve.podDisruptionBudget.maxUnavailable` (1)
  go at once. One replica stays `Recreate`.
- **From 0.35 or before, upgrade with `serve.replicas: 1` first**:
  its pod holds the ledger alone, and refuses new pods until it stops,
  which `Recreate` waits for. Then raise it.
- **No sticky sessions**: with `serve.signIn.oidc`, every pod mounts the
  session key's Secret, which must be one of your own
  (`serve.signIn.sessionKeySecret.name`): one the chart makes is made again
  by a render that can't look it up (`helm template`, Argo CD, Flux), and a
  pod started after would sign with another key. Replicas compare their
  keys' fingerprints: one with a newer key joins, as a rollout brings it
  in, one with an older key than another live is refused. To change the
  key, write the new one into the Secret, then `kubectl rollout restart`
  the Deployment: sessions signed with the old one end, and
  `lapilli_serve_session_keys` says 2 until the rollout is done.
- **`/readyz`** says which pod leads on the metrics port (9091); on the
  UI's port, ok. Give the database to the ledger's role alone (`REVOKE
  CONNECT ... FROM PUBLIC`): a role that may only connect can keep
  replicas from starting.
- **Readiness**: a pod is ready once a run is recorded, as before, and
  while it reaches the database; a pod whose writes wait for the ledger's
  writers' turn stays ready, and says so
  (`lapilli_serve_ledger_writes_waiting`). `/readyz` says which pod leads,
  and `lapilli_serve_leader` is 1 in the leader's metrics, 0 in the others';
  the ServiceMonitor scrapes each pod. What the leader's work counts
  (`lapilli_serve_runs_total`, notices sent) is in its pod's alone.

### Going back after a schema upgrade

A release that changes the ledger's schema says so in the changelog
(**Ledger schema 022**, after 021), and from 0.53 on the new release's
`lapilli preflight` says so beforehand: `serve upgrades the ledger from
021 to 022 as it starts, and a release before 022 can't open it after`.
serve upgrades the ledger as it starts, and a release before can't open
it: it stops as it starts, saying `--ledger: the ledger was written by a
newer lapilli (schema 22; this one knows 21)` (on Postgres,
`--ledger-url-file: ...`), and its pod restarts over and over. So going
back is going back to the ledger as it was, and only a copy taken before
the upgrade gives that.

Before upgrading, take the copy, and upgrade only once it is taken:

- **SQLite, with CSI snapshots**: a `VolumeSnapshot` whose
  `source.persistentVolumeClaimName` is `FULLNAME-ledger`. It is taken in
  the background: upgrade once its `status.readyToUse` is true.
- **SQLite, on a cloud's disk without them**: the cloud's own snapshot of
  the disk behind the claim's PersistentVolume (`kubectl get pv NAME -o
  jsonpath='{.spec.csi.volumeHandle}'`), done before upgrading.
- **SQLite, with neither** (local-path, hostPath): with serve stopped
  (`kubectl -n NAMESPACE scale deploy/FULLNAME --replicas=0`, with a
  GitOps tool kept from scaling it back meanwhile), every file of the
  claim (`ledger.db`, and `ledger.db-wal` and `ledger.db-shm` if there),
  copied out through a pod with a shell that mounts it (`kubectl cp`),
  and that pod deleted before upgrading. Not with `lapilli ledger copy`
  of the new release: it brings the ledger it copies from to its own
  schema first.
- **Postgres**: a backup of the database, or the time to restore it to.

A snapshot of a running disk is to SQLite as a power cut, which it
recovers from; what serve writes between the copy and the upgrade isn't in
the copy, unless serve is stopped for it.

To go back:

1. **Not with `helm rollback`, nor a GitOps tool's history.** They take
   the old revision's values, which have no `existingClaim`: the release
   before mounts the upgraded ledger, and its pod restarts over and over.
   Go back with `helm upgrade --version BEFORE` and the values below, or
   by changing the chart's version and the values together in Git.
2. **SQLite: keep the upgraded claim's disk first.** Once
   `FULLNAME-ledger` isn't rendered, Helm leaves it
   (`helm.sh/resource-policy: keep`), but a GitOps tool that prunes deletes
   it (Argo CD reads `keep` as no more than `Delete=false`; Flux's
   kustomize-controller prunes too), and a reclaim policy of `Delete`
   takes the disk with it. Before anything else, `kubectl patch pv
   "$(kubectl -n NAMESPACE get pvc FULLNAME-ledger -o
   jsonpath='{.spec.volumeName}')" -p
   '{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}'`.
3. **SQLite: a claim of a name of its own from the copy**, such as
   `lapilli-ledger-restore`, never `FULLNAME-ledger` made again: a GitOps
   tool that heals makes a deleted `FULLNAME-ledger` again, empty, before
   the copy is in.
   - From a `VolumeSnapshot`: a claim whose `dataSource` is the snapshot,
     with its `storageClassName`, of at least the snapshot's
     `status.restoreSize` (the old claim's capacity, more than
     `serve.ledger.size` if it was expanded).
   - From the cloud's snapshot: a disk made from it in the zone of the old
     one (a ReadWriteOnce disk attaches to a node in its zone); a
     PersistentVolume of a new name, written from the old one's (`kubectl
     get pv NAME -o yaml`), keeping its CSI driver, capacity,
     `storageClassName` and node affinity, with the new disk's volume
     handle and `persistentVolumeReclaimPolicy: Retain`, and without its
     `claimRef`, `uid`, `resourceVersion`, `creationTimestamp` and
     `status`: a PersistentVolume that keeps the old `claimRef` is held
     for the old claim, and the new one stays Pending. Then a claim that
     names it in `volumeName`, with the same `storageClassName`.
   - From files: a new claim, the files copied in through a pod that
     mounts it and runs as serve does (`runAsUser` and `runAsGroup`
     65532), or `chown -R 65532:65532` them there, and that pod deleted.
     serve runs as 65532, and a hostPath volume, as local-path's are,
     doesn't take `fsGroup`: files `kubectl cp` wrote as another user
     can't be written, and serve stops as it starts.
4. **One change: the release before, and `serve.ledger.existingClaim:
   lapilli-ledger-restore`.** Every chart that runs serve, from 0.6.0,
   takes `existingClaim`, and with it renders no `FULLNAME-ledger`. One
   replica is `Recreate`: the upgraded pod stops before the one before
   starts.
5. **Postgres: the copy restored into a new database**, and a Secret of
   its own (such as `lapilli-ledger-restore`) holding all the old one
   holds, for the new database: its connection string under the same key,
   and the CA the string's `sslrootcert` names. With
   `serve.networkPolicy.egress`, `serve.ledger.postgres.to` and `port` name
   where it is. Then one change: the release before,
   `serve.ledger.postgres.secret.name` naming that Secret, and
   `serve.replicas: 1`, so no pod of the upgraded release still runs beside
   it (one replica is `Recreate`). Raise `serve.replicas` again once it
   runs.
6. **Check it**: the pod is ready, and its log has no `--ledger:` (or
   `--ledger-url-file:`) error; from 0.53 on, `lapilli preflight` with
   serve's flags, where serve runs, says `schema 021, this release's`. The
   first runs replay the hours since the copy, catching up as after any
   stop. What came in after the copy and can't be replayed isn't there:
   incidents, tasks, comments, proposals and settings written in the UI or
   the API, and what was sent to its ports.
7. **The upgraded ledger** is kept: `FULLNAME-ledger` by Helm, its disk by
   `Retain` if a GitOps tool prunes the claim (with prune off, Argo CD
   shows the application OutOfSync while the claim is there). Keep it for
   an upgrade tried again, or, once the release before runs, delete the
   claim and the PersistentVolume, then the disk by hand (under `Retain`
   it outlives them; on local-path, its directory). The upgraded database
   is yours to drop.

Upgrading again later keeps `existingClaim`, or the Secret: that is the
ledger now.

### Teams, quiet windows, reports and roles in Git

```yaml
serve:
  config: |
    teams:
      - {name: payments, rules: ^Checkout, labels: {team: payments}, tz: Asia/Seoul}
    windows:
      - {title: Nightly backup, team: payments, start: 2026-11-02T02:00, end: 2026-11-02T03:00, every: day, rules: ^Backup}
    reports:
      - {name: payments weekly, team: payments, schedule: weekly mon@09:00, dests: [team:payments]}
    grants:
      - {group: payments-oncall, team: payments}
  configPrune: false
  proposalsExpire: 30d
```

`serve.config` declares teams, quiet windows (`windows`), reports and the
editor roles admins give (`grants`), each with the fields the UI's form for
it takes (`values.yaml` lists them all), rather than having them made in
the UI. Give it as a text, a block scalar, as above: the chart renders it
as written. Given as a mapping, Helm reads it as YAML 1.1 first, and
writes `no`, `off`, `y` as `false` or `true`, `0123` as `83`, `1.10` as
`1.1`; lapilli refuses a name, a title, a user or a group that reaches it
as a boolean or a number, and a label's value as a boolean or a fraction,
but can't tell `83` from `0123`. Times stay as written either way.

The chart renders it into a ConfigMap, `FULLNAME-config`, mounted as a
directory and passed as `--config`; lapilli keeps the ledger holding what
it says when a pod starts to lead, and again whenever the file changes,
without a restart: the kubelet updates the file within a minute or so,
and lapilli reads it every 15 seconds. **A file is kept to only once the
same file has been read twice in a row**, 15 seconds or more apart, with
no other read between: a file read half written, or swapped in part,
changes nothing, giving no one a role and taking none away. So a new
file is kept to 15 seconds or more after it is first read, and a pod that
starts to lead keeps the ledger to it 15 seconds after it has read the
rules: usually within half a minute, but that is no bound. The Settings
page says when a file waits, and why (a ConfigMap changing back and
forth, or a ledger write that failed). With several replicas, the leader
alone writes it, in one transaction. Each thing is matched by its name (a
team's in any letter case, a quiet window's by its title); what the UI made
of the same team under that name is taken over. What `serve.config`
declares is marked declared in the UI, which doesn't change or delete it,
nor does the API: change it here. A file that can't be read (YAML that
isn't, a key it doesn't know, an empty file, `grants:` or a role's `team:`
with nothing after it: leave a key out for every team) changes nothing,
and is said at once; a thing in it that can't be kept (a report to a
destination nothing names, a team that isn't there) is kept as it was, and
the rest is kept. A team that can't be kept keeps what names it in the
file as it was too. The Settings and Run history
pages say what can't, and to an admin why, with the line. An admin's
`/config.yaml` gives everything the ledger holds in this form, declared
and made in the UI alike, to paste under `serve.config`.

Taken out of `serve.config`, a thing is the UI's again, as it was, but a
role (`grants`). **A role is taken away only when a file that was read
twice no longer lists it**, as a file is kept to only then. Declaring a
role gives it to the file, as Argo CD's RBAC policy owns its roles. With
`serve.configPrune`, the rest is deleted then too; but a team that
something still names (a window, a report, a role, a task, a note) is
given back to the UI instead. Without `serve.config` (left `null`, or the
chart rolled back to before it), everything a file declared is the UI's
again, roles included, none taken away and nothing deleted, each marked
given back: until the UI changes one, a `serve.config` put back that
doesn't declare it again takes it away (a role), or deletes it with
`serve.configPrune`. The Settings page lists them. The mark doesn't
expire: to keep one, change it in the UI, or keep it on the Settings page
(an admin's, one or all); a role can't be changed, so delete it and give
it again, or keep it there. What the file declares
has limits of its own beside the UI's; given back past the UI's own, a
thing is deleted. What the file no longer declares goes before what it
adds is made. To rename a team
in place, keeping what names it, give the new `name` with the old as
`renamed_from`, and change every `team:` that names it, in one change:
renamed by its name alone, it is another team.

`serve.config` declares goals too (`goals:`, `docs/design/goals.md` in
the repository): what the alerting is held to, each a bound on one of the
summary's numbers, for everyone, a team or each team (`team: "*"`), each
whole week met, missed or not known on the Goals page, the overview, the
digest, reports and `/metrics`; or, with `each: KIND`, each occurrence of
a kind of marked window, such as a sale, beside the four weeks before it
with `no_worse_than: before`. Only the file declares a goal: one it no
longer declares is deleted, with or without `serve.configPrune`.
`serve.goalTasks` makes a task, of kind decide, for each goal a whole week
missed, 20 a run at most (`--goal-tasks`); it needs `serve.config`.

```yaml
serve:
  config: |
    goals:
      - {name: critical-caught, measure: critical.share_caught, at_least: 0.8}
      - {name: none-missed, measure: incidents.missed, at_most: 0}
      - {name: no-refire, team: "*", measure: refired.firings, at_most: 0}
  goalTasks: true
```

`serve.proposalsExpire` closes a proposal that waited for approval longer
since it began to wait (made, or backtested again), such as `30d`, and
that no one approved, as expired, so its rule can have another. Empty, the default, keeps it waiting however
long. Neither goes with `serve.readOnly`, which the chart refuses.

## Pull request backtests

`serve.backtest` opens a port of its own that takes pull request backtests
from CI jobs, and nothing else: `POST /v1/backtests`, and `GET` and
`DELETE /v1/backtests/ID`. A job sends the rules before and after a pull
request, and serve replays them with its own access to the history, judges
them by its record, and gives back the comment `lapilli verdict --compare
--changed-only --markdown` writes: CI holds no credential for the metrics,
and the backtests and the hourly runs share one quota. The design is
[docs/design/backtests.md](https://github.com/lapilli-project/lapilli/blob/main/docs/design/backtests.md), and what serve
does with it [docs/serve.md](https://github.com/lapilli-project/lapilli/blob/main/docs/serve.md#pull-request-backtests).
It runs PromQL the pull request writes, with serve's access: give that
access what the rules need and no more. Off unless enabled; it needs
`prometheus.url` or `gcpProject` and rules serve replays, and is refused
with `serve.readOnly`, which keeps the queue's writes out.

```yaml
serve:
  backtest:
    enabled: true
    trust:
      configMap: {name: lapilli-backtest-trust}   # key: trust.yaml
    tokens:
      - {secretName: lapilli-backtest-azure, key: token}
```

- `serve.backtest.trust.configMap` is the ConfigMap holding
  `--backtest-trust`'s file under `key` (`trust.yaml`), mounted at
  `/etc/lapilli/backtest/`: who may ask, each entry a CI's issuer of ID
  tokens, with the audience and the claims its tokens must hold, or a plain
  token. It is required, and read as serve starts: one serve can't use
  stops it from starting, and a change takes a restart (`kubectl rollout
  restart deployment/FULLNAME`).

  ```yaml
  trust:
    - name: alerts
      issuer: https://token.actions.githubusercontent.com
      audience: https://lapilli-ci.example.com
      claims: {repository_id: ["123456789"], event_name: pull_request}
    - name: monitoring-azure
      token_file: /var/run/lapilli/backtest/tokens/lapilli-backtest-azure/token
  ```

  ```sh
  kubectl -n monitoring create configmap lapilli-backtest-trust --from-file=trust.yaml
  ```

- `serve.backtest.tokens` are the Secrets holding the plain tokens the
  trust file's `token_file` entries name, for a CI with no ID token
  (Azure Pipelines, Gitea, Jenkins): each `{secretName, key}` (`key`
  `token` when left out) is mounted at
  `/var/run/lapilli/backtest/tokens/SECRET/KEY`, and read for each
  request, so a token is rotated without a restart. A token is 32
  characters at least, and none may be another port's
  (`serve.incidentsAPI`, `firings`, `trackers`, `notificationsAPI`,
  `changes`): the chart refuses the same Secret and key, serve the same
  token.
- `serve.backtest.port` (8084) is the container's, `--backtest-listen`;
  `share` (0.25, 0.05 to 0.75, of `--max-rate`), `queue` (20, 1 to 1000)
  and `timeout` (15m, 1m to 2h) are `--backtest-share`, `--backtest-queue`
  and `--backtest-timeout`, the defaults when empty.

The port is apart from the UI's: it takes neither the UI's sign-in nor its
host check, each request carrying a token of its own, and a request a page
sent (with an `Origin`) is refused. It is on `FULLNAME`, which has every
port, and on a Service of its own, `FULLNAME-backtest`, this port alone,
which `serve.backtest.service` and `serve.backtest.ingress` serve as
`serve.service` and `serve.ingress` serve the UI's. Where that fits:

1. **Runners inside the cluster or the network.** `FULLNAME-backtest` is a
   ClusterIP by default: runners in the cluster reach it at
   `http://FULLNAME-backtest.NAMESPACE.svc:8084`, in plain http, and nothing
   new is exposed. The recipes take plain http to
   `NAME.NAMESPACE.svc` or `NAME.NAMESPACE.svc.cluster.local` alone, and
   through no proxy: `FULLNAME-backtest` alone is looked up under the
   runner's search domains, which needn't be the cluster's, and
   `NAME.NAMESPACE` could be a name on the internet, as a namespace named
   as a top-level domain is. Their ID token's audience is, as a rule, the
   URL they send to: a trust entry of their own with `audience:
   http://FULLNAME-backtest.NAMESPACE.svc:8084`, beside one with the
   external URL for runners elsewhere. The recipes refuse an audience that
   isn't `LAPILLI_SERVE`'s URL unless `SERVE_AUDIENCE_ANY=1` (the action's
   `audience-any: true`): set it for runners here that ask for the
   external URL's audience. With `serve.networkPolicy`,
   `serve.backtest.from` names them, as NetworkPolicy peers, and lets them
   in to this port alone.
2. **A CI provider's own runners**, with the port exposed on its own,
   apart from the UI and whatever signs people in to it, and only over
   TLS, which lapilli doesn't serve: the chart refuses it otherwise.
   - `serve.backtest.ingress`: an Ingress for `FULLNAME-backtest` alone
     (`className`, `annotations`, `hosts`, `tls`), its paths
     `/v1/backtests` and below. It needs `tls`, or, for TLS ended at the
     load balancer in front with a certificate of its own (an ALB's
     `alb.ingress.kubernetes.io/certificate-arn`, a GKE managed
     certificate), `loadBalancerEndsTLS: true` saying so.
   - `serve.backtest.service.type: LoadBalancer` (or `NodePort`), with
     `annotations`, `loadBalancerClass`, `loadBalancerSourceRanges`,
     `externalTrafficPolicy`, `nodePort` and `port`, for a load balancer
     that ends TLS (an AWS Network Load Balancer's TLS listener, an OCI
     load balancer's), which `loadBalancerEndsTLS: true` says. GKE's and
     AKS's load balancers for a Service are TCP, and don't: use the
     Ingress there.

   The port answers nothing but its API, so a load balancer's HTTP health
   check of `/` fails: point it at `/healthz` on `serve.metricsPort` (on
   GKE, a BackendConfig's `healthCheck.port` and `requestPath`, named by
   `cloud.google.com/backend-config` in `serve.backtest.service.annotations`;
   on EKS, `alb.ingress.kubernetes.io/healthcheck-port` and
   `healthcheck-path`), and with `serve.networkPolicy` let the health
   checks in through `metricsFrom`. `serve.backtest.from` then names the
   ingress controller's pods, or the load balancer's addresses as
   `ipBlock`s.

Where it doesn't: serve reached only through an identity-aware proxy, a VPN
or a private network, with a provider's runners, which can't pass the proxy
without a credential of the proxy's own (a cloud identity, a long-lived
secret: what this was to save). There, keep the recipe running Lapilli in
CI, or a runner inside. Letting runners in by address is no fence for
GitHub-hosted runners, whose published ranges are many and change; runners
with fixed addresses can be let in so (`loadBalancerSourceRanges`, an
`ipBlock` in `from`), beside the token.

To check a CI's ID tokens, serve fetches each issuer's discovery document
and keys: with `serve.networkPolicy.egress`, its `to` must let serve reach
them over https, port 443, as it must an identity provider.

`lapilli preflight` with serve's flags, where serve runs, says before a
change is rolled out whether serve takes the trust file or stops at it
(`backtest.trust`), whether each issuer's discovery document and keys
answer as a token's check needs (`backtest.issuers`), and how much a
backtest fits in `timeout` at `share` (`estimate.backtests`); with
`--offline`, in CI, the trust file is read all the same, the plain
tokens' Secrets not (docs/design/preflight.md).

## Who people are


By default no one signs in, and the Service is a ClusterIP: reach the UI
with `kubectl port-forward`. To serve it at a URL people open, sign them
in, one way of three (`docs/design/signin.md` in the repository):

- `serve.signIn.oidc`: lapilli's own sign-in, with any OpenID Connect
  provider, as Grafana and Argo CD have. Register lapilli as a client whose
  redirect URI is `serve.externalURL` followed by `/auth/callback`, and give
  `issuer`, `clientId` and, for a confidential client, `clientSecret` (a
  Secret). A browser is sent to the provider and comes back with a session
  in a signed cookie; `POST /auth/logout` ends it. An API client or an
  agent (MCP) sends an ID token the provider gave this client as a bearer
  token (`bearer: false` takes none). `hostedDomains` checks Google's
  Workspace domain; `trustedClients` are the provider's other clients whose
  tokens are taken. The pod must reach the provider: with
  `serve.networkPolicy.egress`, name it in `to`. Lapilli asks it at start,
  and a wrong `issuer` is an ERROR in its log.
- `serve.signIn.proxyJWT`: the JWT a sign-in in front signs, checked
  against its keys: `google-iap` (Google Cloud's Identity-Aware Proxy;
  `audience` is the backend service's,
  `/projects/NUMBER/global/backendServices/ID`), `aws-alb` (an Application
  Load Balancer's sign-in; `albArn` is the load balancer's), or `jwks` (any
  other: oauth2-proxy with `--pass-authorization-header`, an Azure App
  Service's sign-in, an access proxy; `header`, `jwks`, `issuer`,
  `audience`). A request without it, or with one that doesn't verify, gets
  401: reaching the pod around the proxy gets nowhere. A program let
  through IAP or an ALB comes with the same header as a person's browser:
  keep programs out of the roles that approve.
- `serve.userHeader`: the plain header a proxy that signs people in puts
  the person in, such as `X-Forwarded-Email`. Anyone who reaches the pod
  can set it, so it is refused without `serve.networkPolicy` (below), unless
  `serve.networkPolicy.othersKeptOut` says something else keeps the rest
  out; and with a LoadBalancer or a NodePort, which no proxy sits in front
  of, unless `serve.service.loadBalancerSourceRanges` names the proxy's
  addresses alone and `serve.service.sourceRangesAreTheProxy` says so.

With `serve.signIn`, only those a role names come in, and everyone else
gets 403: an identity provider signs in whoever it knows, which for a
public one (Google's for any account, a client whose consent screen is
External; GitLab.com; GitHub through Dex without an organization; a
multi-tenant Microsoft Entra ID app) is anyone. Name who may look in
`serve.roles.viewers` (people, `group:NAME`, `*@DOMAIN`), and who may
change in `editors` and `admins`; a sign-in with no role list is refused.
`'*'` in `editors` or `admins` is everyone `viewers` lets in, and refused
until `viewers` names who. There is no anonymous viewing with a sign-in.

`serve.signIn.userClaim` (`email`) and `groupsClaim` (`groups`) are the
claims that name the person and list their groups; with `oidc`, `email`
needs `email_verified` beside it, so take `preferred_username` or `oid` from
a provider that doesn't send it (Microsoft Entra ID), and with any sign-in
`email_verified: false` is refused. The design note has a table of what
each provider sends (GitLab's `groups_direct`, Keycloak's group paths,
Entra ID's group overage). Sessions last `sessionMaxAge` (12h), signed with
a key whoever holds can sign in as anyone: by default a Secret the chart
makes, `FULLNAME-session-key`, kept across upgrades and when the release
is uninstalled (delete it to sign everyone out), or one of yours,
`sessionKeySecret.name`. Give one of yours when a GitOps tool renders the
chart without the cluster (Argo CD, Flux): it would make a new key each
time.

- `serve.roles` limits who may change what, by the name the sign-in gives
  and the groups it lists (`groupsHeader` with `userHeader`). `editors`
  write; `admins` also change the settings and the teams; everyone else may
  only look. `'*'` is everyone signed in.
- Without `serve.roles`, everyone may only look: lapilli serve listens on
  the pod's address, which every pod in the cluster reaches. With no
  sign-in, `serve.everyoneAdmin` lets everyone who reaches the UI or the
  API change anything, as an admin; it is off by default, and refused
  beside `serve.roles`, `serve.readOnly` or `serve.signIn`. Before 0.28.1,
  everyone was an admin without roles, `kubectl port-forward` included:
  that is now `serve.everyoneAdmin`.
- `serve.networkPolicy` lets in only those in `from` to the UI's port (the
  proxy, the ingress controller's or the Gateway's pods, a load balancer's
  addresses), and Prometheus (`metricsFrom`) only to the metrics port.
  - The probes and the ServiceMonitor use the metrics port,
    `serve.metricsPort` (9091). The readiness probe is `/readyz`, ready once
    the first run is done (`serve.readinessPath: /healthz` for at once); the
    liveness probe is `/healthz`.
  - `serve.networkPolicy.egress` limits where the pod may connect, in a
    NetworkPolicy of its own: off by default, then the cluster's DNS and
    the egress rules in `to` alone. Name each place it talks to: the
    Prometheus, the rules' API, the rules history's repository, Slack and
    the webhooks, a cloud's APIs and its identity's token endpoint, the
    identity provider. A cloud's metadata server (169.254.169.254) and the
    Kubernetes API are then out of reach unless named; a workload identity
    that answers from the metadata server (GKE's) needs it named, as
    Google's documentation on network policies says.
- `serve.allowedHosts` names the host your proxy or ingress passes on, if
  it isn't `serve.externalURL`'s. Other names get 421, against DNS
  rebinding.

## Serving it at a URL

- `serve.service.type`: `LoadBalancer` or `NodePort` puts the UI alone on a
  Service of its own, `FULLNAME-ui`, on `serve.service.port` (80), with
  `annotations`, `loadBalancerClass`, `loadBalancerSourceRanges`,
  `externalTrafficPolicy` and `nodePort`. `FULLNAME` stays a ClusterIP with
  every port, as jobs, alert managers, the recipes and Prometheus reach it.
- `serve.ingress`: an Ingress (`className`, `annotations`, `hosts`, `tls`,
  `https`).
- `serve.httpRoute`: a Gateway API HTTPRoute (`parentRefs`, `hostnames`,
  `https`).

`serve.externalURL` is worked out from the Ingress's first host or the
HTTPRoute's first hostname when not given, https unless their `https` is
false (TLS is often ended in front of an Ingress, which then has no `tls`),
and their hosts are added to `serve.allowedHosts`, so the host check and
the sign-in's redirect hold. Lapilli is served at the root of its host: a
path is refused. Serving the UI outside the cluster with no sign-in is
refused, since anyone who reaches it reads the ledger, unless
`serve.exposeWithoutSignIn` says it is meant.

`/readyz`, the readiness probe, is ready once the ledger has a run
recorded: at once after a restart; on a first install after the
backfill's first day, which `helm install --wait` and Argo CD's health wait
for (`serve.readinessPath: /healthz` doesn't).

`serve.signIn.oidc` needs https, but on this machine (a name under
`.localhost`): end TLS at an Ingress or a Gateway, or at a load balancer
that ends it (an AWS Network Load Balancer's TLS listener, an OCI load
balancer's). GKE's and AKS's load balancers for a Service are TCP, and
don't.

A load balancer's health checks reach `/healthz` on the UI's port without a
sign-in. With `serve.networkPolicy`, `from` must let in what the load
balancer sends from: the ingress controller's or the Gateway's pods by
their namespace, or, for a LoadBalancer Service, its addresses and its
health checks as `ipBlock`s (the clients' own with `externalTrafficPolicy:
Local`). Each cloud, alike:

- **GKE.** A LoadBalancer is an external passthrough Network Load Balancer;
  `networking.gke.io/load-balancer-type: "Internal"` makes it internal.
  GKE's Ingress (`className: gce` or `gce-internal`) and Gateway check
  health on the serving port: point them at `/healthz` (a BackendConfig's
  `healthCheck.requestPath`, named by the Service's
  `cloud.google.com/backend-config` annotation, in
  `serve.ingress.serviceAnnotations`, or a HealthCheckPolicy).
  Health checks come from `35.191.0.0/16` and `130.211.0.0/22`.
  Identity-Aware Proxy turned on in the BackendConfig or a GCPBackendPolicy
  goes with `serve.signIn.proxyJWT.kind: google-iap`; the audience is the
  backend service's number and id.
- **EKS.** With the AWS Load Balancer Controller, a LoadBalancer with
  `loadBalancerClass: service.k8s.aws/nlb` and
  `service.beta.kubernetes.io/aws-load-balancer-scheme: internet-facing` (or
  `internal`) is a Network Load Balancer; an Ingress with `className: alb`
  an Application Load Balancer (`alb.ingress.kubernetes.io/target-type:
  ip`, `alb.ingress.kubernetes.io/healthcheck-path: /healthz`,
  `alb.ingress.kubernetes.io/certificate-arn`). Its sign-in
  (`alb.ingress.kubernetes.io/auth-type: oidc` or `cognito`) goes with
  `serve.signIn.proxyJWT.kind: aws-alb` and the load balancer's ARN. With
  targets by IP, traffic comes from the load balancer's addresses in the
  VPC's subnets.
- **AKS.** A LoadBalancer is an Azure Load Balancer;
  `service.beta.kubernetes.io/azure-load-balancer-internal: "true"` makes it
  internal, and
  `service.beta.kubernetes.io/azure-load-balancer-health-probe-request-path:
  /healthz` points its probe at the health check, which comes from
  `168.63.129.16`. The application routing add-on's Ingress
  (`className: webapprouting.kubernetes.azure.com`) or Application Gateway
  for Containers' Gateway carry the UI by host. With Microsoft Entra ID as
  the OIDC provider, the issuer is
  `https://login.microsoftonline.com/TENANT/v2.0`, and `userClaim` is
  `preferred_username` (Entra doesn't send `email_verified`); its `groups`
  claim lists group object IDs.
- **OKE.** A LoadBalancer is an OCI Load Balancer
  (`service.beta.kubernetes.io/oci-load-balancer-shape: flexible`), or a
  Network Load Balancer with `oci.oraclecloud.com/load-balancer-type: nlb`;
  `service.beta.kubernetes.io/oci-load-balancer-internal: "true"` makes it
  internal, and `loadBalancerSourceRanges` becomes its security rules. The
  OCI Native Ingress Controller, or any ingress controller, carries the UI
  by host. An OCI IAM identity domain is an OIDC provider like any other.
- **Anywhere else** (bare metal, kind): MetalLB or cloud-provider-kind give
  a LoadBalancer its address; any ingress controller or Gateway API
  implementation carries the UI by host.

`serve.notify` tells what happens to a webhook, whose URL is kept in a
Secret, and to Slack. The events are tasks made, assigned, done or overdue,
incidents written down, quiet windows kept, and comments. Links point under
`serve.externalURL`. `serve.notify.webhookSecret.signingKey` names a key of
the same Secret holding a key of 32 bytes or more: each post to the webhook
is then signed with it, `X-Lapilli-Timestamp` and `X-Lapilli-Signature`, for
the receiver to check that it came from Lapilli, as
[Signed posts to the webhook](https://github.com/lapilli-project/lapilli/blob/main/docs/serve.md#signed-posts-to-the-webhook)
says.

`serve.config` decides who is told what too, under `notify:`: routes send
each event by its kind (`task.created`, or a family, `task.*`), its team and
its labels to destinations by name, the first that takes it deciding
unless it says `continue: true`, and what none takes to `default`, which
routes need, so that nothing is dropped. The names are `webhook`, `slack`,
`team:NAME` (`serve.teamDigests.teams`), `serve.reports.destinations`'
names, and `serve.notify.webhookDests`', JSON webhooks of their own, each
URL in a Secret's key and, with `signingKey`, each signed with its own key;
never a URL. `templates:` gives a destination words of its own, from
`{{.Event}}`, `{{.Line}}`, `{{.Link}}`, `{{.Rule}}`, `{{.Team}}`,
`{{.Title}}`, `{{.ID}}`, `{{.By}}`, `{{.Labels}}` and `{{.At}}`, each value
escaped for where it goes. Every replica reads the routes, as it tells of
what it writes; the Settings page shows them, read-only, and how each
destination's notices went. Without routes, every event goes to the
webhook and, with `serve.notify.slack`, Slack, as before
([Who is told what](https://github.com/lapilli-project/lapilli/blob/main/docs/design/notifications.md#who-is-told-what)).

```yaml
serve:
  notify:
    webhookDests:
      - {name: payments-hook, secretName: lapilli-payments-hook, key: url, signingKey: key}
  config: |
    notify:
      routes:
        - kinds: [task.*, proposal.*]
          teams: [payments]
          to: [team:payments, payments-hook]
      default: [slack]
```

Agents reach `/mcp` (MCP) and read what the pages show. `serve.mcpCompare`
(`editors` or `admins`) lets their agents replay a change to the rules they
propose. It is off by default, since it runs PromQL the agent writes, with
lapilli's access to Prometheus, and for a Loki's rules LogQL, with lapilli's
access to Loki, and it needs `serve.roles`.
`serve.mcpPropose` lets their agents propose a change to a rule, for a
person to approve; an agent never approves or applies one.

`lapilli serve` reads by default, and changes only what a person approves.
`serve.changes.enabled` turns changes on: a change an approver approved in
the UI is checked again, then opened as a pull request by a recipe in
[recipes/changes](https://github.com/lapilli-project/lapilli/tree/main/recipes/changes),
which holds the token in `serve.changes.tokenSecret` and reaches the
`incidentsAPI` port, or, for rules read from Grafana, applied through its
API with `serve.changes.grafanaTokenSecret`. It needs a sign-in
(`serve.signIn` or `serve.userHeader`), roles, `serve.roles.editors` or `serve.roles.admins` (without them everyone
is an admin, who could resume changes), and someone to approve: the
admins, unless `serve.changes.approvers` says who; `approvals: 2` asks for
two people (an undo is approved by the one approver who asks for it). The Secrets are mounted only
when changes are on. To stop changes at once, pause them in the UI, or make
a ConfigMap with a key `paused` named after the chart's full name: the
release's name followed by `-lapilli`, unless it has `lapilli` in it
already, then `-changes-paused`. For a release named `ops`:

```sh
kubectl create configmap ops-lapilli-changes-paused --from-literal=paused=1
```

For one named `lapilli`, `lapilli-changes-paused`. `helm install` prints
the command for the release. Deleting the ConfigMap resumes changes.

For rules read from a ruler (`rules.ruler.url`), a change is applied
through the ruler's own API, and only in the namespaces, or Azure rule
groups, `serve.changes.in` names: name none a tool keeps (mimirtool rules
sync, Terraform, Alloy's `mimir.rules.kubernetes`), as it would undo the
change. A ruler's token reaches every namespace of its tenant, so
`serve.changes.in` is Lapilli's own check. The write credentials, one a
kind of ruler, are used only for writes:

- Mimir, Cortex: `serve.changes.rulerTokenSecret`, a token that may write
  rules (`rules:write`); `rules.ruler.tokenSecret`'s reads them.
- Amazon: `serve.changes.awsRole`, a role the pod's identity assumes (its
  own role needs `sts:AssumeRole` on it, and the role's trust policy lets
  it), allowed only this, on the namespaces named:

  ```json
  {
    "Version": "2012-10-17",
    "Statement": [{
      "Effect": "Allow",
      "Action": ["aps:PutRuleGroupsNamespace", "aps:DescribeRuleGroupsNamespace"],
      "Resource": [
        "arn:aws:aps:REGION:ACCOUNT:rulegroupsnamespace/WORKSPACE_ID/shop",
        "arn:aws:aps:REGION:ACCOUNT:rulegroupsnamespace/WORKSPACE_ID/solo"
      ]
    }]
  }
  ```

  The pod's own role reads them with `aps:ListRuleGroupsNamespaces` and
  `aps:DescribeRuleGroupsNamespace` on the workspace.
- Azure: `serve.changes.azureClientId`, the client ID of a second
  user-assigned identity federated with the same service account, given
  this custom role, assigned on each rule group named:

  ```json
  {
    "Name": "Lapilli rule group writer",
    "IsCustom": true,
    "Actions": [
      "Microsoft.AlertsManagement/prometheusRuleGroups/read",
      "Microsoft.AlertsManagement/prometheusRuleGroups/write",
      "Microsoft.Authorization/permissions/read",
      "Microsoft.Authorization/denyAssignments/read",
      "Microsoft.Authorization/locks/read"
    ],
    "AssignableScopes": ["/subscriptions/SUB/resourceGroups/RG"]
  }
  ```

  It has no `delete`: Lapilli never deletes a rule group. The three
  `Microsoft.Authorization` reads are `lapilli preflight`'s, below; serve
  doesn't need them.

`lapilli preflight` (docs/design/preflight.md) says whether each write
identity may make these writes, asked without writing: for the Amazon role,
IAM's policy simulator, which takes **`iam:SimulatePrincipalPolicy` on the
role's own ARN** (it shows the role only what its own policies allow; serve
never needs it, and without it `changes.aws` is unknown); for Azure's
identity, its permissions, the deny assignments and the locks at the
resource group, or at the rule group named, and the subscription's locks,
which take `Microsoft.Authorization/permissions/read`,
`denyAssignments/read` and `locks/read` (without them, `changes.azure` is
unknown), and its read of what each rule group names (the workspace, an
action group).

Without one, a Mimir's or a Cortex's writes are sent with no token, for a
ruler reached only inside the cluster, and Amazon's and Azure's with the
pod's own identity. The Amazon and Azure paths were tested against
stand-ins answering as their API documents show, not against an account.

A cloud's own alarms (`rules.alarms`) are read with the pod's own
identity, whatever `prometheus.auth` says, every `rules.alarmsEvery` (10m
by default):

- CloudWatch: `cloudwatch:DescribeAlarms` on `*` (composite alarms are
  described only so) and `cloudwatch:ListTagsForResource`.
- Azure: Monitoring Reader on the subscription or resource group.
- Google Cloud: `roles/monitoring.viewer` on the project.
- OCI: `inspect alarms`, `read alarms` and `read metrics` in the
  compartment (`GetAlarm` asks for `METRIC_READ` in the alarm's metric
  compartment), through OKE workload identity, a resource principal or an
  instance principal.

With changes on, an approved change to an alarm is applied through its
cloud's API, only where `serve.changes.in` names it as `SOURCE:NAME`
(CloudWatch's alarm name, Azure's `RG/NAME`, Google's policy's display
name, OCI's display name; `NAME*` for a prefix), and never to one tagged as
another tool's. Name none that CloudFormation, Terraform, Bicep, Pulumi or
Config Connector keeps. An alarm Lapilli can't replay (not PromQL) is
retired as far as off, never deleted from here. Turning an alarm off, or
deleting it, waits while it fires: CloudWatch's alarm state, Azure's
alerts (Alerts Management, `Microsoft.AlertsManagement/alerts/read`, in
Monitoring Reader), Google's alerts (`monitoring.alerts.list`, in
`roles/monitoring.viewer`), OCI's alarm status (`ListAlarmsStatus`, under
the `inspect alarms` and `read metrics` above), each read with the pod's
own identity just before the write. Each cloud's write identity, used only
for writes, is given only this:

- CloudWatch: `serve.changes.awsRole`, for each region and prefix named:

  ```json
  {
    "Version": "2012-10-17",
    "Statement": [
      {
        "Effect": "Allow",
        "Action": [
          "cloudwatch:PutMetricAlarm",
          "cloudwatch:DisableAlarmActions",
          "cloudwatch:EnableAlarmActions",
          "cloudwatch:DeleteAlarms",
          "cloudwatch:ListTagsForResource",
          "cloudwatch:TagResource"
        ],
        "Resource": "arn:aws:cloudwatch:REGION:ACCOUNT:alarm:checkout-*"
      },
      {
        "Effect": "Allow",
        "Action": "cloudwatch:DescribeAlarms",
        "Resource": "*"
      }
    ]
  }
  ```

  `DeleteAlarms` and `TagResource` are for a PromQL alarm deleted four
  weeks after it was turned off, and made again with its tags to undo it.
  No `PutCompositeAlarm`, EC2, Auto Scaling or `iam:CreateServiceLinkedRole`:
  the alarms that would need them aren't changed. CloudWatch's writes name
  an alarm by its name, in the caller's account, so a write is refused
  unless the alarm's ARN is in the role's account (without
  `serve.changes.awsRole`, the pod's own, as STS `GetCallerIdentity` says;
  it needs no permission, but it needs STS: a cluster that reaches
  CloudWatch alone, through its VPC endpoint, say, fails every write
  without the role, so give it STS's endpoint too, or set the role).
- Azure: `serve.changes.azureClientId`, a second user-assigned identity
  federated with the same service account, given this custom role on each
  alert rule named, or on the resource group for `RG/*`:

  ```json
  {
    "Name": "Lapilli alert rule changes",
    "IsCustom": true,
    "Actions": [
      "Microsoft.Insights/metricAlerts/read",
      "Microsoft.Insights/metricAlerts/write",
      "Microsoft.Insights/metricAlerts/delete",
      "Microsoft.Insights/scheduledQueryRules/read",
      "Microsoft.Insights/scheduledQueryRules/write",
      "Microsoft.Insights/activityLogAlerts/read",
      "Microsoft.Insights/activityLogAlerts/write",
      "Microsoft.Insights/actionGroups/read",
      "Microsoft.Authorization/permissions/read",
      "Microsoft.Authorization/denyAssignments/read",
      "Microsoft.Authorization/locks/read"
    ],
    "AssignableScopes": ["/subscriptions/SUB/resourceGroups/RG"]
  }
  ```

  Only a PromQL metric alert is ever deleted, so only `metricAlerts` has
  `delete`. Azure Monitor asks, as an alert rule is written, for "read
  permission on the target resource of the alert rule" and "on any action
  group associated to the alert rule" (alerts-create-metric-alert-rule):
  `actionGroups/read` here, assigned also where the action groups are,
  and read on each rule's targets (Reader, or Monitoring Reader, on
  them). The three `Microsoft.Authorization` reads are `lapilli
  preflight`'s; serve doesn't need them.
- Google Cloud: `serve.changes.googleServiceAccount`, a service account the
  pod's identity impersonates (it holds
  `roles/iam.serviceAccountTokenCreator` on that one account, and nothing
  more), given on the project this custom role:

  ```yaml
  title: Lapilli alerting policy changes
  stage: GA
  includedPermissions:
  - monitoring.alertPolicies.get
  - monitoring.alertPolicies.list
  - monitoring.alertPolicies.update
  - monitoring.alertPolicies.delete
  - monitoring.alertPolicies.create
  ```

  Google grants these on a project only, so `serve.changes.in` is
  Lapilli's own check there. `create` is for a PromQL policy deleted and
  made again to undo it.
- OCI: `serve.changes.ociConfigSecret`, a Secret holding an OCI config
  file (`configKey`) and the API key it names (`keyKey`), for a user in a
  group of its own; its `key_file` is `/etc/lapilli/oci/key.pem`. OCI's
  workload identity has no second identity a pod can take for writes, so
  this one is a key, mounted only when changes are on. The group's policy,
  for each compartment named:

  ```
  Allow group lapilli-alarm-changes to manage alarms in compartment shop
    where any {request.permission = 'ALARM_INSPECT', request.permission = 'ALARM_READ',
               request.permission = 'ALARM_UPDATE'}
  Allow group lapilli-alarm-changes to read metrics in compartment shop
  ```

  `manage` is the only verb with `ALARM_UPDATE`; the condition leaves out
  the rest. OCI's alarms aren't PromQL, so Lapilli never deletes or makes
  one again.

`lapilli preflight` says whether each of these may make the writes, asked
without writing (docs/design/preflight.md): CloudWatch's role through IAM's
policy simulator, which takes `iam:SimulatePrincipalPolicy` on the role's
own ARN (without it, `changes.aws` is unknown); Azure's identity by its
permissions, the deny assignments and the locks at the resource group of
each alert rule named (one given on the alert rule alone reads as
unknown), and its read on what each rule names, with the three
`Microsoft.Authorization` reads in the role above; Google's service
account, or without it the pod's own identity, by Cloud Resource
Manager's `testIamPermissions` on the project, which needs no
permission; OCI's user by its groups and the policies of each alarm's
compartment and its metric's compartment and those above them, which
takes `inspect groups`, `inspect users`, `inspect compartments` and
`inspect policies` in the tenancy (give `read metrics` where each
alarm's metric is, its `metricCompartmentId`, which may be another
compartment than the alarm's). The OCI policy above has a condition,
which `lapilli preflight` never counts as a grant: it says `changes.oci`
is unknown, "a policy with a condition".

None of the four was run against an account: each was tested against a
stand-in answering as its API documents show.

A release's chart runs its image by digest: `image.digest`, which the
release workflow writes with the digest of the image it built and attested,
as it does the chart's `lapilli.dev/image-digest` annotation. With
`image.tag` another version, give that version's `image.digest` too, or
empty it: the chart refuses another tag with its own digest, which would
run its own image under another version's name.

`serve.deploys` names a ConfigMap holding the file `--deploys` reads, and
beside it the histories it names, what `recipes/versions/argocd.sh` or
`flux.sh` write for an Application, a Kustomization or a HelmRelease: each
deploy of the service or namespace its labels say. A firing that came during
one (agreeing with it on every label both have), from `before` (0) before
its start to `after` (10m) after its end, is marked, judged as ever, and
shown apart: a column and an order on the rules, a tile on the overview,
the digest, `/metrics` and `/api/v1/summary`, with precision both ways.
`serve.marked` names a ConfigMap holding the file `--marked` reads, and
`serve.mark` the PromQL expressions (`--mark`), each a `kind`, an `expr`
that returns something while a window of the kind is on, and the labels of
its series its windows keep (`match`, none by default): marked windows, a
sale, a launch, a campaign (docs/design/marked.md). A firing that came
during one is marked with its kind, judged as ever and never excused, and
every count is read inside the kind and outside it: the Marked windows
page, the rules list (`during:sale`), a rule's page, the overview, reports
and the digest, `/metrics` and `/api/v1/summary`. `serve.config`'s
`marked:` declares them too.
`serve.checkContent.runbookAnnotations` names the annotations a runbook is
read from, the first a rule has; `serve.checkContent.runbookBase`, an https
URL a runbook that is a path in the rules' repository is a link under.

`serve.serviceMonitor.enabled` makes a ServiceMonitor, which honours
lapilli's labels (`honorLabels`: a rule's `team` stays `team`, not
`exported_team` beside a target label of that name) and refuses a scrape of
more than `serve.serviceMonitor.sampleLimit` (50,000) series; and
`dashboard.enabled` a ConfigMap holding a Grafana dashboard for Grafana's
sidecar, of what an operations team measures its alerts by: critical
firings that caught something, precision, re-fires and the whole weeks
with one, firings during deploys and precision without them, what caught
nothing by when it came, impact windows and incidents caught, caught only
by noisy rules or missed, and the lead of their first alert, each rule's
scorecard, and how long since each source last brought something. Its
variables pick a team, severities, and the severities counted as critical.
`test/e2e/dashboard.sh` asks each of its queries of a real Grafana.

Webhook URLs and tokens are read from Secrets and never shown.
See [values.yaml](https://github.com/lapilli-project/lapilli/blob/main/charts/lapilli/values.yaml)
for every setting and [lapilli.dev](https://lapilli.dev) for how the verdict
is reached.
