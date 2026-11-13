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

`serve.teamDigests` posts a team its own weekly digest. `serve.reports.destinations`
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
PersistentVolumeClaim is made. It is still one replica, replaced with
`Recreate`: one process writes a ledger at a time, and a second is refused
the ledger's lock, which is a session's, so connect directly or through a
pooler in session mode, not transaction mode. With
`serve.networkPolicy.egress`, `serve.ledger.postgres.to` names where the
database is, as NetworkPolicy peers, and `port` its port (5432). A SQLite
ledger moves there once, with serve stopped: `lapilli ledger copy --ledger
DIR --to-url-file FILE`, from a pod that mounts both.

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
`serve.externalURL`.

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
two people. The Secrets are mounted only
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
      "Microsoft.AlertsManagement/prometheusRuleGroups/write"
    ],
    "AssignableScopes": ["/subscriptions/SUB/resourceGroups/RG"]
  }
  ```

  It has no `delete`: Lapilli never deletes a rule group.

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
      "Microsoft.Insights/activityLogAlerts/write"
    ],
    "AssignableScopes": ["/subscriptions/SUB/resourceGroups/RG"]
  }
  ```

  Only a PromQL metric alert is ever deleted, so only `metricAlerts` has
  `delete`.
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
