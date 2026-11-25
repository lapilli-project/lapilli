package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lapilli.dev/internal/ledger"
	"lapilli.dev/internal/prom"
	"lapilli.dev/internal/report"
	"lapilli.dev/internal/scenario"
	"lapilli.dev/internal/serve"
	"lapilli.dev/internal/signin"
	"lapilli.dev/internal/source"
	"lapilli.dev/internal/verdict"
)

const serveUsage = `lapilli serve keeps judging: every hour it replays the rules over the hours
since it last did, records the firings, the incidents and every version of
the rules in a ledger, and shows the stones in a web UI, a JSON API and
Prometheus metrics. Incidents can be written down in the UI, and judged
against at once.

Usage:
  lapilli serve [flags]

Where the firings come from (pick one), what went wrong, where the history
is, and how to judge: as for lapilli verdict (lapilli verdict -h), except:
  --rules PATH        one file or directory, read again every --watch: each
                      change is recorded as a new version, from when it was
                      seen, and replayed as such
  --on-call FILE      as for lapilli verdict, each feed read again every so
                      often (every: in the file), its turns kept in the
                      ledger past what the feed holds
  --rules-select SEL  as for lapilli verdict, for --rules and --rules-history:
                      what it and the documents that hold no rules leave
                      out of a file isn't kept in the ledger with it
  --rules-history DIR earlier versions of those rules, one directory each,
                      named by when it took effect (2026-10-14T16:40:31Z or
                      20261014T164031Z), so a first start's backfill replays
                      each day with that day's rules; a ledger with versions
                      of its own passes them over. recipes/versions/git.sh
                      writes one from git. Of each version, only its
                      documents of rules are kept in the ledger.
                      Without --rules, it is the rules, followed: its
                      newest version is in effect, read again every
                      --watch, and each version added to it later, as
                      recipes/versions/follow.sh adds them, is recorded
                      from its own time, the hours since replayed again
  --loki-rules PATH   one file or directory of Loki ruler rules, read again
                      every --watch, beside the rest
  --incidents FILE    read again each run; optional, as incidents can be
                      written down in the UI
  --responses FILE    the pages a paging tool sent, and whether a person
                      acknowledged each and who resolved it, read again each
                      run: responses: {id, alerts, sent, acknowledged,
                      resolved, resolved_by}; recipes/responses writes one.
                      A firing is answered, unanswered or changed, and
                      useful by it (docs/responses.md)
  --responses-token-file SOURCE=FILE
                      a token (16 characters at least, not the others', not
                      another source's) that may PUT
                      /api/v1/responses/SOURCE?from=&to= on --incidents-listen:
                      replace what that source says of the pages sent in
                      that period (31 days at most, 16 MiB of JSON); it
                      writes no other source's. Once for each source; the
                      --responses file's pages outrank an API source's;
                      recipes/responses/put.sh
  --rollouts          also read, from the history at --prometheus, the
                      rollouts, rollbacks and scales kube-state-metrics
                      shows for the namespaces the firings are in, each run
  --rollout-match LABEL
                      a label whose value is the workload's name, tying a
                      firing to it (repeatable; goes with --rollouts)
  --routine P         a change is routine, chance and not a response, at or
                      above this chance (default 0.5)
  --noisy-by caught|useful
                      what a rule is found noisy by, to start with: the
                      Settings page changes it (default useful once
                      --responses or --rollouts is given, else caught,
                      with --deploys alone too)
  --deploys FILE      when services and namespaces were deployed, read again
                      each run: a list of deploys, each the labels that say
                      what was deployed (service, namespace) and either a
                      history recipes/versions/argocd.sh or flux.sh wrote
                      (REVISION TIME a line, beside the deploys file or
                      below it) or a start and an end. A firing that starts
                      during one, from --deploy-before before its start to
                      --deploy-after after its end, and agrees with it on
                      every label both have, one at least, is marked as
                      during a deploy: judged as ever, and shown apart, so
                      precision can be read with those firings and without
                      (docs/design/noc.md). Read again only when it or a
                      history changed
  --deploy-before DUR, --deploy-after DUR
                      how long before a deploy's start, and after its end, a
                      firing still counts as during it (default 0 and 10m;
                      a day at most each)
  --marked FILE       marked windows, read again each run: spans of a kind a
                      team names (a sale, a launch, a campaign), each a kind,
                      a title, a start and an end, repeating or not, of some
                      labels or a team's. A firing that starts during one
                      and agrees with it is marked with its kind: judged as
                      ever, never excused, and every count read inside the
                      kind and outside (docs/design/marked.md)
  --mark KIND=PROMQL  an expression that returns something while a window of
                      KIND is on, such as shop_sale_active == 1, read each
                      run as --impact is; its windows mark every firing.
                      Repeatable, eight at most
  --mark-match KIND=LABEL,...
                      the labels of each series of KIND's expression its
                      windows keep: they mark the firings that agree with
                      them (none by default)
  --runbook-base URL  an https URL a runbook that is a path in the rules'
                      repository (docs/runbooks/disk.md#full) is a link
                      under, such as
                      https://git.example.com/org/monitoring/blob/main/:
                      the rule's page links to it
  --lead, --match, --refire, --noisy-firings, --noisy-precision
                      the defaults; the UI can change them
  --match-strict      a firing that lacks a --match label the incident has
                      doesn't catch it; changed at a start, every firing is
                      judged again with it
  --linked-only       an incident that lists the alerts its tool linked to
                      it is caught by those alone; changed at a start,
                      every firing is judged again with it

The ledger:
  --ledger DIR        where the ledger is kept (default lapilli-ledger)
  --ledger-url-file FILE
                      keep the ledger in Postgres instead: the file holds
                      its connection string, as pgx reads it
                      (postgres://USER:PASSWORD@HOST:5432/DB?sslmode=verify-full),
                      read at start and said nowhere. Several serves on
                      one Postgres ledger are replicas: one leads, and runs
                      what is run once; each serves (docs/design/replicas.md).
                      "lapilli ledger copy" moves a SQLite ledger there
                      (docs/design/postgres.md)
  --every DUR         run this often (default 1h)
  --settle DUR        and replay the last DUR before the last run's end
                      again, for points that arrived late (default 1h)
  --backfill DUR      on first start, replay this far back (default 28d, at
                      most 3650d)
  --keep DUR          drop firings older than this (default 400d; 0 keeps
                      everything)
  --keep-activity DUR drop the activity log's changes older than this, never
                      sooner than --keep (default 800d, or --keep when
                      longer; 0 keeps them all, as with --keep 0)
  --watch DUR         read the rules this often (default 1m)
  --alarms-every DUR  read each --alarms this often (default 10m), so the
                      clouds' request quotas hold
  --digest DAY@HH:MM  once a week, judge the week before, work out longer
                      "for"s for noisy rules, and shorter ones that would
                      have caught the incidents nothing caught, and post the
                      report to --slack-webhook-file (default mon@09:00, in
                      --tz). Once an admin sets the digest on the Reports
                      page or at /api/v1/reports/digest, that is used
                      instead, and the flags only name where it can go
  --tz ZONE           the zone days and weeks are counted, and times shown,
                      in (default: local)
  --timeout DUR       give up a run after this long (default 50m)

Sharing the history with the alerts, so that Lapilli never takes their
quota (docs/design/quota.md). /metrics counts what this process asks of
each history API (lapilli_serve_history_*). Off by default:
  --yield-on-429      when a history API (--prometheus, --loki, Grafana's
                      data source proxy) answers a run's request with HTTP
                      429, ask it nothing more in the run, not even again
                      after a wait. The run stops there, and so does what
                      reads the history after it that round (reviews of
                      changes, the digest and what follows it, backtests
                      again, changes carried out, the replay set beside what
                      really fired), for a later round. The rules it didn't
                      replay are replayed first by a later run, from where
                      they were missed, and the runs go no further than a
                      week past the oldest of them, so none is given up; the
                      run history says why of each. Without it, a 429 is
                      asked again, as --max-rate says
  --yield-for DUR     and ask that API nothing for DUR after the 429, a day
                      at most (default 0: the next run asks again). While
                      --prometheus is left alone, runs wait, held in the run
                      history. What was yielded to is this process's: a new
                      leader asks afresh
  --guard EXPR        a PromQL expression asked of --prometheus before each
                      run and between the days of a catch-up, such as how
                      often the alerts' own evaluations fail
                      (rate(prometheus_rule_evaluation_failures_total[5m]) >
                      0): while it returns anything, or can't be read, the
                      run waits for the next --every, and so does what reads
                      the history after it, held in the run history. One the
                      history refuses as written fails the run, each time
  --catch-up-hours HH:MM-HH:MM
                      catch up only in these hours, in --tz, such as
                      01:00-06:00, at least --every long (twice is better): a
                      run more than a day behind where the ledger's firings
                      are up to (the first start's --backfill, one after
                      being down a day or more; not one replaying again from
                      when a version of the rules took effect) waits for
                      them, held. Runs are recorded in order, so the hourly
                      runs resume once it caught up; a run less behind runs
                      at any hour. Hours a change of clocks skips (02:00-03:00
                      on the night it does) don't come that day
  --catch-up-rate N   while catching up (more than a day behind), ask each
                      history API N requests a second at most, besides
                      --max-rate
  --check-replay=false
                      don't set the replay beside what really fired. It is
                      on with rules to replay: each day the runs won't
                      replay again is set beside ALERTS (or GRAFANA_ALERTS),
                      and a rule whose replay differs says so
  --check-series SEL  the series that say what really fired (repeatable;
                      default ALERTS and GRAFANA_ALERTS, as the rules need).
                      Without it, with --grafana or --check-grafana, what
                      Grafana's rules really fired is read from Grafana's
                      own alert state history instead, where it keeps one it
                      serves: in annotations, its default, from Grafana 12.4
                      on, or in Loki; GRAFANA_ALERTS first, while it holds
                      anything, where Grafana may keep it too. A day it can't
                      be read for, Grafana's rules aren't checked, and the
                      check goes on
  --check-grafana URL the Grafana that runs the Grafana rules --rules or
                      --rules-history gives, whose alert state history says
                      what they really fired, read with --grafana-token-file
  --grafana-history=false
                      don't judge the rules --grafana holds that can't be
                      replayed by Grafana's own alert state history. It is on
                      with --grafana, where it keeps one it serves: each
                      round, what it says they fired up to where the runs got
                      is copied in under the source grafana, which no job may
                      write to, and each such rule is judged by it, under its
                      own name, as a cloud's alarm is by its history
  --check-differs N/RATIO
                      a rule differs when, over a week, it had N firings
                      either way and fewer than RATIO of them were alike
                      (default 5/0.9)
  --check-content=false
                      don't check what the rules say when they fire. It is
                      on with rules to read: each rule's page says what it
                      says and what to fix in it, the rules list marks those
                      with a problem of fact, and /metrics has
                      lapilli_rule_content_problems. With --grafana,
                      Grafana's notification templates, contact points and
                      policies are read once an hour too, with the same
                      token, and checked beside each rule
  --alertmanager-config FILE|URL
                      as for lapilli verdict --check-content: Alertmanager's
                      configuration, read once an hour, and its templates
                      checked beside each rule it is sent; a file that
                      can't be read, or an API that refuses, leaves the
                      check as it was, and the log says why once
  --severities LIST, --scrape-interval DUR
                      as for lapilli verdict --check-content; without
                      --scrape-interval, Prometheus's /api/v1/status/config
                      and /api/v1/targets are asked once a day, for the
                      global interval and each job's, and the jobs the
                      series of a selector that names none come from: the
                      range check counts with them, and a replay asks
                      Grafana's reduces that don't count points at the
                      shortest interval the series they read are scraped
                      at, or, where they aren't served, at Grafana's own
                      resolution (1s)

The web UI and API:
  --listen ADDR       where to listen (default 127.0.0.1:8080). Without a
                      sign-in (below), keep it inside, or behind a proxy
                      that signs people in. Listening beyond this machine
                      (:8080, a pod's address) with no roles, everyone may
                      only look
  --metrics-listen ADDR
                      also serve /metrics, /healthz and /readyz here, alone,
                      so a NetworkPolicy can let Prometheus reach only them
  --shutdown-delay DUR
                      told to stop (SIGTERM, Ctrl-C), go on answering on
                      every port for this long before they close, /readyz
                      answering 503, so that a Service's endpoints or a load
                      balancer send requests elsewhere first; then the
                      requests under way finish, 10s at most (default 0, at
                      most 10m; the chart gives 5s to more than one
                      replica). The leader's work stops at once either way
  --incidents-listen ADDR
                      also take incidents here, and nothing else, from a job
                      that holds the token in --incidents-token-file: PUT or
                      DELETE /api/v1/incidents/ID, POST /api/v1/incidents.
                      A NetworkPolicy can let that job reach this port alone,
                      and not the one whose headers name people
  --incidents-token-file FILE
                      the file holding that token, 16 characters at least;
                      read for each request, so it can be rotated
  --firings-token-file FILE
                      also take firings recorded elsewhere, such as a
                      cloud's own alarm history, on --incidents-listen,
                      from a job that holds this token: PUT
                      /api/v1/firings/SOURCE?from=&to= with the firings
                      that start in [from, to), one JSON object a line as
                      --firings reads them (recipes/firings/put.sh sends a
                      recipe's), in place of those the ledger has from
                      SOURCE then. With it, --rules (or the like) and
                      --prometheus are optional: without them nothing is
                      replayed, and what is copied in is judged as it comes
  --source-quiet DUR  tell, once, of a source nothing has been copied in from
                      for this long, to --notify-webhook-file and, with
                      --notify-slack, Slack: the job copying it may have
                      stopped (default 26h; give a weekly job's 8d)
  --tasks-token-file FILE
                      also let a job keep tasks in step with a tracker, on
                      --incidents-listen, with this token: GET
                      /api/v1/tasks?linked=false (or true)&status=open,doing
                      &after=ID, and PATCH /api/v1/tasks/ID with a task's
                      link (an https URL, set once) and status, nothing else
                      (recipes/trackers)
  --allowed-hosts LIST
                      host names to answer to, comma-separated, besides an
                      address, localhost, a name without dots or under
                      .svc, .cluster.local or .internal, and --external-url's:
                      the name a proxy passes on, if it is another. Any
                      other name is refused, against DNS rebinding; * for
                      any
  --user-header NAME  the header such a proxy names the person in, such as
                      X-Forwarded-Email; changes are recorded under it. Set it
                      only behind a proxy that sets it and strips it from
                      callers
  --read-only         take no changes through the UI or API
  --config FILE       a YAML file declaring teams, quiet windows (windows),
                      reports and roles (grants), as the UI's forms make
                      them, which the ledger is kept holding: at start, and
                      whenever its content changes, read again every 15s.
                      What it declares the UI shows and doesn't change; GET
                      /config.yaml gives what the ledger holds in its form.
                      A file that can't be used changes nothing, a thing it
                      declares that can't be kept is kept as it was, and the
                      Settings and Run history pages say why
                      (docs/design/serveops.md). Under notify:, routes:
                      who is told what, by kind, team and labels, and a
                      default (docs/design/notifications.md)
  --config-prune      delete what --config no longer declares, once the file
                      is read the same twice in a row, but a team something
                      the UI made names; without it, that is the UI's again.
                      A goal (goals:) the file no longer declares is deleted
                      either way
  --goal-tasks        make a task, of kind decide, for each goal --config
                      declares that a whole week missed, or an occurrence
                      of a goal over a period (each: KIND), 20 a run at
                      most; one open for a goal and team is commented on
                      as weeks (occurrences) are missed again, met, or no
                      longer missed (docs/design/goals.md). Off unless
                      turned on
  --expect FILE       an expectations file, as lapilli verdict --expect
                      takes, that the rules are held to after deploy: after
                      each run, each rule's budgets and catches are checked
                      over the week so far and the twelve whole weeks
                      before, shown on its page, the overview and the
                      digest, and told once a rule, an expectation and a
                      week when broken. Read again every 15s, as --config;
                      a file that can't be read changes nothing. --config's
                      expectations: and a rule's annotation
                      lapilli.dev/budget hold rules too
                      (docs/design/expectations.md)
  --expect-tasks      make a task, of kind tune, for the rule's team, of
                      each expectation broken, once a rule, an expectation
                      and a week, 20 a run at most, none while one is open.
                      Off unless turned on
  --promtool PATH     run the scenarios the expectations hold rules to
                      (scenarios:, promtool's own test files, read in the
                      directory of the file that declares them) with
                      promtool at PATH, against each rule's group as the
                      rules in effect have it, again when either changes:
                      5 a run, 30s each, with no environment and its
                      rule_files the group alone. Only rules in
                      Prometheus's format. Without it, each scenario is not
                      known. Scenarios a pull request's backtest sends
                      (--backtest-listen) are never run
  --promtool-memory SIZE
                      the memory one run of --promtool may take, as 256Mi
                      or 1Gi, 64Mi at least (default 256Mi): on Linux,
                      promtool's RLIMIT_DATA, past which the run stops and
                      the scenario is not known; promtool runs in serve's
                      container, so leave room for it under its memory
                      limit. Not set on macOS, where promtool runs without
                      a cap
  --team-labels LIST  the labels whose values the Teams page suggests as
                      teams, when the rules carry values no team owns by,
                      comma-separated (default team,owner,squad)
  --everyone-admin    with no roles (--editors, --admins), let everyone who
                      reaches the UI and API change anything, as an admin,
                      though it listens beyond this machine. Without roles,
                      and without it, only a server listening on loopback
                      (127.0.0.1, localhost) takes changes; beyond it, every
                      pod or machine that shares its network could
  --viewers LIST      who may look, and with a sign-in of lapilli's own who
                      may come in at all, beside those the other lists name:
                      people, group:NAME, *@DOMAIN (everyone whose name ends
                      in @DOMAIN), or * for everyone signed in. With it,
                      everyone else is refused, 403
  --editors LIST      with this or --admins, only those named may change
                      anything, and the rest may only look: a comma-separated
                      list of people (as --user-header names them) and groups
                      (as --groups-header lists them), *@DOMAIN, or * for
                      everyone signed in (with a sign-in of lapilli's own,
                      only once --viewers names who may come in). Editors write incidents, tasks, comments,
                      quiet windows and notes on rules. user:NAME is a
                      person and group:NAME a group, alone: use them where
                      someone could pick a name that is a group's
  --admins LIST       as --editors, who may also change the settings, the
                      teams, the reports and the weekly digest, and give an
                      editor's role, of every team or of one, on the Roles
                      page or at /api/v1/roles: never an admin's or an
                      approver's, which stay these flags'
  --groups-header NAME
                      the header such a proxy lists the person's groups in,
                      separated by commas, such as X-Forwarded-Groups. With
                      roles, nothing but the proxy should reach lapilli
                      serve, and it should strip both headers from callers

Signing people in, without a proxy of plain headers (one of these, or
--user-header; docs/design/signin.md). Every page and API call then needs
someone signed in, but /healthz, /readyz and /static/, and only those a
role names come in at all: --viewers, --editors, --admins, --approvers,
or a role given in the UI. An identity provider signs in whoever it
knows, which for a public one (Google's for any account, GitLab.com,
GitHub through Dex, a multi-tenant Entra ID app) is anyone:
  --oidc-issuer URL   sign people in with this OpenID Connect provider, its
                      configuration discovered from the URL (https, or http
                      to this machine or a name inside a cluster): the
                      authorization code flow, with PKCE, state and nonce.
                      A browser is sent to sign in, and keeps a session in a
                      signed cookie; an API client or an agent sends an ID
                      token the provider gave this client as a bearer
                      token. Needs --external-url (https, or http to this
                      machine), whose /auth/callback the provider sends
                      people back to
  --oidc-client-id ID the client lapilli is registered as
  --oidc-client-secret-file FILE
                      the file holding the client's secret, read for each
                      sign-in; without it, a public client (PKCE alone)
  --oidc-scopes LIST  the scopes asked for, comma-separated (default
                      email,profile, and openid always); add groups where
                      the provider lists them only when asked
  --oidc-hosted-domain LIST
                      Google Workspace domains people must come from (the
                      hd claim), comma-separated
  --oidc-trusted-clients LIST
                      the provider's other clients whose ID tokens (azp)
                      are taken as this one's, comma-separated; others
                      are refused
  --oidc-bearer=false take no ID token as a bearer token: sessions alone
  --user-claim NAME   the claim that names the person (default email; with
                      --oidc-issuer, email_verified must be true beside it)
  --groups-claim NAME the claim that lists their groups, for --editors,
                      --admins and --approvers (default groups; empty for
                      none)
  --session-max-age DUR
                      how long a session lasts, and the oldest a bearer
                      token is taken at, from its iat, whatever its own exp
                      (default 12h; 5m to 30d)
  --session-key-file FILE
                      a key of 32 characters or more that session cookies
                      are signed with, read at start; without it, one made
                      for the process, so sessions end when it restarts.
                      Whoever holds the key can sign in as anyone; a new
                      one signs everyone out
  --proxy-jwt KIND    take who each request comes from from the JWT a
                      sign-in in front signed, checked against its keys:
                      google-iap (Google Cloud's Identity-Aware Proxy,
                      x-goog-iap-jwt-assertion), aws-alb (an AWS Application
                      Load Balancer's sign-in, x-amzn-oidc-data), or jwks
                      (any other: oauth2-proxy, an Azure App Service's
                      sign-in, an access proxy). A request without it, or
                      with one that doesn't verify, gets 401
  --proxy-jwt-audience AUD
                      google-iap: the backend service's,
                      /projects/NUMBER/global/backendServices/ID; jwks: the
                      aud the JWTs carry
  --proxy-jwt-alb-arn ARN
                      aws-alb: the load balancer whose signature is taken
  --proxy-jwt-issuer URL
                      jwks: the iss the JWTs carry; aws-alb: the identity
                      provider's, to check it too
  --proxy-jwt-jwks URL
                      jwks: where the keys are
  --proxy-jwt-header NAME
                      jwks: the header the JWT is in; Authorization takes it
                      as a bearer token

Telling of what happens (tasks made, assigned, done or overdue; incidents
written down; quiet windows kept; comments):
  --notify-webhook-file FILE
                      the file holding a URL to POST each, as JSON, to; for
                      a tracker, a chat or anything else that takes a
                      webhook. Sent apart from the writes, and tried again,
                      further apart, for about eight hours when it fails
  --notify-webhook-secret-file FILE
                      the file holding a key, 32 bytes at least (a line break
                      at its end isn't of it), to sign each post to
                      --notify-webhook-file with: X-Lapilli-Timestamp, the
                      Unix seconds it is sent at, and X-Lapilli-Signature,
                      sha256= and the HMAC-SHA256 of the timestamp, a dot
                      and the body, in hex. Read for each post, so it can be
                      rotated
  --notify-slack      also post each, as a line, to --slack-webhook-file
  --external-url URL  where people reach the UI, such as
                      https://lapilli.example.com; what is told links to it
  --lang en|es|ja|ko  the language of what is posted to Slack (default en)
  --team-digest TEAM=FILE
                      also post each week's digest for one team, its own
                      rules and incidents, to the Slack-style incoming webhook
                      whose URL is in FILE (repeatable). TEAM is a team's name
                      in the ledger. --notify-webhook-file gets the digest as
                      digest.ready, for everyone and for each team
  --report-dest NAME=FILE
                      a destination the reports people schedule in the UI can
                      go to: the Slack-style incoming webhook whose URL is in
                      FILE (repeatable). NAME is lowercase letters, digits and
                      . _ -. Reports can also go to slack (--slack-webhook-
                      file), webhook (--notify-webhook-file, as
                      report.ready) and team:TEAM (each --team-digest)
  --webhook-dest NAME=FILE
                      a destination routes in --config's notify: can send
                      to: the webhook whose URL is in FILE, posted JSON as
                      --notify-webhook-file is (repeatable). NAME is
                      lowercase letters, digits and . _ -, none that
                      --report-dest gives. It takes only what a route
                      sends it; needs --config
  --webhook-dest-secret NAME=FILE
                      the file holding a key, 32 bytes at least, to sign
                      each post to --webhook-dest NAME with, as
                      --notify-webhook-secret-file signs the webhook's
                      (repeatable)

Notifications as they were sent, kept beside the firings: how many each rule
sent, how many repeats, how many woke people, how many for nothing:
  --notify-listen ADDR
                      take notifications here, and nothing else, each source
                      on a path of its own: POST /v1/alertmanager (a webhook
                      with the token as its bearer token),
                      /v1/alertmanager/events (Alertmanager's event
                      recorder, 0.33.0 on: every delivery, PagerDuty's and
                      Slack's too, the same token; ?skip=NAME passes over a
                      receiver that is Lapilli's own webhook), /v1/grafana (a
                      webhook contact point, with the token or an HMAC
                      signature), /v1/sns (Amazon SNS, for Amazon Managed
                      Service for Prometheus), /v1/azure (an action group's
                      webhook, common alert schema, the token as the URL's
                      password), /v1/google (a Cloud Monitoring webhook
                      channel, with basic auth). ?receiver=NAME names the
                      receiver where the notification doesn't
  --notify-token-file FILE
                      the file holding that token, 16 characters at least;
                      read for each request, so it can be rotated
  --notify-grafana-secret-file FILE
                      the file holding Grafana's HMAC secret, 16 characters
                      at least. Set Grafana's timestamp header to
                      X-Grafana-Alerting-Timestamp
  --notify-sns ARNS   the SNS topics taken, by ARN, comma-separated. A
                      subscription to one is confirmed by fetching its
                      SubscribeURL, from an SNS host only: the one request
                      the port makes

Changing rules, with a person's approval (off by default: Lapilli reads,
and proposes; docs/design/changes.md):
  --changes           carry out the changes a person approved: each checked
                      again (the rule read again, the backtest run again),
                      then handed to a recipe that opens a pull request where
                      the rule is kept (recipes/changes/), or, for a rule kept
                      only in Grafana, a ruler or a cloud's own alarms,
                      applied through its API with what it replaces kept for
                      undo. Turning a rule off, or deleting it, waits while
                      it fires. Lapilli also
                      proposes, after each week's digest: longer fors for
                      noisy rules, thresholds and fors for near misses,
                      retiring spare rules and those that never fired. Needs
                      a sign-in (--user-header, --oidc-issuer or
                      --proxy-jwt), roles (--editors or --admins: without
                      them, whoever may change anything is an admin) and
                      who approves (--approvers or --admins)
  --approvers LIST    who may approve a change, as --editors lists people
                      and groups (default: the admins). Approving is a form
                      on the proposal's page, in a browser: never the API,
                      MCP or a token. No one approves their own
                      proposal, or one their agent made
  --approvals N       how many people approve each change: 1 (default) or
                      2. An undo of an applied change is the exception: the
                      one approver who asks for it approves it, ticking the
                      box that they read the change, as an approval does
  --changes-token-file FILE
                      the change recipes' token, on --incidents-listen: GET
                      /api/v1/changes and PATCH /api/v1/changes/ID, what
                      became of a pull request, and nothing else
  --changes-grafana-token-file FILE
                      a Grafana service account's token that may write the
                      alert rules Lapilli may change, for --grafana. An
                      Editor's changes a for or what a rule says; turning a
                      rule off takes an Admin's, as Grafana keeps a pause
                      from an Editor (answering that it took it)
  --changes-in LIST   the namespaces of --ruler Lapilli may change rules in,
                      comma-separated, each a name or a prefix ending in *
                      (team-*); on Azure, rule groups, RG/NAME, or NAME when
                      --ruler names one resource group, in any letter case.
                      Loki's, with --loki-loaded-rules, as loki:NAMESPACE
                      (loki:shop, loki:team-*). And a cloud's alarms,
                      SOURCE:NAME, an --alarms source
                      and an alarm in it: CloudWatch's alarm name, Azure's
                      RG/NAME (in any letter case), Google Cloud's policy's
                      display name, OCI's display name (cloudwatch-prod:
                      checkout-*). Without it nothing in a ruler or a
                      cloud is changed. Name none that a tool keeps
                      (mimirtool rules sync, Terraform, CloudFormation,
                      Config Connector, Alloy's mimir.rules.kubernetes): it
                      would undo the change, or fight it. A ruler's token,
                      and Google Cloud's and OCI's permissions, reach more
                      than these: this is Lapilli's own check
  --changes-managed KEY=VALUE
                      a tag that means another tool keeps an Amazon
                      namespace, an Azure rule group or a cloud's alarm,
                      KEY=* for any value (repeatable), an OCI defined tag
                      as NAMESPACE.KEY=VALUE: one carrying it is for a
                      person to change. CloudFormation's
                      aws:cloudformation:stack-id, and Google Cloud's
                      managed-by-cnrm=true and goog-terraform-provisioned=true,
                      are known without it
  --changes-ruler-token-file FILE
                      a token that may write a Mimir's or a Cortex's rules
                      (rules:write), for --ruler; without it, writes are sent
                      with no token, for a ruler reached only in the cluster
  --changes-loki-token-file FILE
                      a token that may write the rules of --loki's ruler,
                      read with --loki-loaded-rules, sent with --loki-tenant
                      as its reads are; without it, writes are sent with no
                      token, for a Loki reached only in the cluster. A
                      Loki whose ruler reads its rules from local files
                      (ruler.storage.type: local) takes no writes
  --changes-aws-role ARN
                      a role the pod's identity assumes to write Amazon's
                      rule groups namespaces, allowed
                      aps:PutRuleGroupsNamespace and
                      aps:DescribeRuleGroupsNamespace on the namespaces
                      named, and CloudWatch's alarms (see the chart's
                      README); without it, the pod's identity writes
  --changes-azure-client-id ID
                      the client ID of a user-assigned identity to write
                      Azure's rule groups and alert rules as, federated
                      with the same service account and given a role that
                      reads and writes those named; without it, the pod's
                      identity writes
  --changes-google-service-account EMAIL
                      a service account the pod's identity impersonates
                      (IAM Credentials' generateAccessToken) to write Google
                      Cloud's alerting policies, given a role that reads,
                      updates, deletes and creates them; without it, the
                      pod's identity writes
  --changes-oci-config FILE
                      an OCI config file, and the API key its key_file
                      names, of a user to write OCI's alarms as, in a group
                      allowed to update them in the compartments named;
                      without it, the pod's identity writes
  --changes-limits D,C
                      at most D deletions a day and C changes an hour
                      (default 5,20); a change past them waits
  --changes-paused FILE
                      while this file is there, nothing is carried out or
                      handed out; an admin can also pause changes in the UI
  --proposals-expire DURATION
                      close a proposal no one approved that waited longer
                      since it was made, such as 30d, as expired, so its rule
                      takes another; 0, the default, keeps it waiting

Agents, over MCP at /mcp:
  --mcp-compare editors|admins
                      who an agent must act for to replay a change to the
                      rules it proposes (the compare tool). Off by default:
                      it runs PromQL the agent writes, with lapilli's access
                      to Prometheus, and, for a rule kept in Loki (--loki),
                      LogQL the agent writes, sent to Loki, or through
                      Grafana's data source proxy, with lapilli's token. It
                      needs roles (--editors or --admins). The tools that
                      read are everyone's
  --mcp-propose editors|admins
                      who an agent must act for to propose a change to a
                      rule (the propose tool), kept as an agent's for a
                      person to approve. An agent never approves or applies

Pull request backtests, from CI jobs (off by default; docs/serve.md, "Pull
request backtests"):
  --backtest-listen ADDR
                      take pull request backtests here, and nothing else:
                      POST /v1/backtests from a CI job, with the rules before
                      and after (recipes/backtest's serve mode), replayed by
                      the replica that leads, one at a time, with this
                      serve's access to the history and judged by its
                      record; GET /v1/backtests/ID, with the key given, says
                      where it is and, once done, gives the comment lapilli
                      verdict --compare --changed-only --markdown writes;
                      DELETE cancels it. Prometheus-format rules only, 50
                      that differ at most, each selector of one metric by its
                      name. It runs PromQL the pull request writes, with this
                      serve's access: give that access what the rules need
                      and no more. Needs --backtest-trust
  --backtest-trust FILE
                      who may ask, read at start: a YAML file of entries
                      under trust:, each a CI's issuer of ID tokens (issuer,
                      audience, the claims each token must hold: the
                      repository's id, as one or a list, and the event, for
                      GitHub Actions, GitLab CI and Forgejo Actions) or a
                      plain token (token_file, 32 characters at least, read
                      for each request), with days (14) and hourly (30). One
                      that can't be used stops serve from starting
  --backtest-share F  of --max-rate, the share the backtests ask the history
                      at, besides the runs' own pace, so that together they
                      never pass it (default 0.25; 0.05 to 0.75). Without
                      --max-rate, a quarter of --parallel at once, one at
                      least
  --backtest-queue N  backtests waiting at most, 3 of an entry and one of a
                      pull request, a newer commit's taking the place of the
                      one waiting (default 20; 1 to 1000)
  --backtest-timeout DUR
                      one backtest's time, from its start (default 15m; 1m
                      to 2h): one that couldn't finish in it at the lane's
                      rate is refused before it is queued, saying what
                      period would fit

Pages: / (overview), /rules, /incidents, /firings, /settings, /runs, /teams,
/windows, /tasks, /proposals, /outcomes. API: /api/v1/report?from=&to=
(&format=csv: the firings a row each, docs/numbers.md; &redact=true: every
name an alias, as lapilli verdict --redact gives),
/api/v1/incidents, /api/v1/rules, /api/v1/runs, /api/v1/settings,
/api/v1/teams, /api/v1/notes, /api/v1/windows, /api/v1/tasks,
/api/v1/comments, /api/v1/proposals, /api/v1/me, /api/v1/outcomes,
/api/v1/summary?from=&to=&team= (each week, for everyone and each team:
critical firings and the share that caught something, re-fires, off hours,
during deploys, incidents and impact windows caught and missed, the median
lead; docs/design/noc.md). MCP: /mcp. Also /metrics, /healthz, /readyz.
`

// serveSetup is what lapilli serve's flags say, as serve reads them: its
// configuration, and the flags it shares with lapilli verdict.
type serveSetup struct {
	cfg serve.Config
	f   *verdictFlags
	// trustFile is --backtest-trust; trustErr why serve.ReadTrust refused
	// it, which serve stops at, and lapilli preflight says as an item
	// (readServeAs).
	trustFile string
	trustErr  error
}

// readServe reads lapilli serve's flags, and refuses a flag, or a
// combination of them, that serve refuses: lapilli preflight reads them
// so too. With secretsLater (lapilli preflight --offline, run where serve's
// secrets aren't), the files of secrets are taken as given, unread: a
// token, a key, a webhook's URL, the ledger's connection string.
func readServe(args []string, secretsLater bool) (serveSetup, error) {
	return readServeAs(args, secretsLater, false)
}

// readServeAs is readServe; with trustSaid (lapilli preflight), a trust
// file serve.ReadTrust refuses, as serve refuses it, is kept in the setup,
// for the report to say, not refused as a flag is: it is a file serve
// reads, as --config and --incidents are.
func readServeAs(args []string, secretsLater, trustSaid bool) (serveSetup, error) {
	cfg := serve.Config{
		Ledger: "lapilli-ledger", Listen: "127.0.0.1:8080", Every: time.Hour, Settle: time.Hour,
		Backfill: 28 * 24 * time.Hour, Keep: 400 * 24 * time.Hour, KeepActivity: 800 * 24 * time.Hour, Watch: time.Minute,
		Digest: serve.Weekly{Day: time.Monday, Hour: 9}, Version: buildVersion(), PromtoolMemory: defaultPromtoolMemory,
	}
	timeoutSet := false
	promtoolMemorySet := false
	ledgerSet := false
	keepActivitySet := false
	yieldForSet := false
	deployAfter, deployBefore, deployTimes := 10*time.Minute, time.Duration(0), false
	var marks, markMatches []string
	// The sign-in of lapilli's own, read from the flags that give it.
	var (
		oc               signin.OIDC
		px               signin.Proxy
		ocSet, pxSet     bool
		claims           = signin.Claims{UserClaim: "email", GroupsClaim: "groups"}
		claimsSet        bool
		sessionSet       bool
		oidcScopesGiven  bool
		externalURLLocal bool
	)
	// secret reads a secret's file, there reports whether a file is: each
	// as given, with secretsLater.
	secret := func(path string, least int) ([]byte, error) {
		if secretsLater {
			return []byte(strings.Repeat("x", max(least, 1))), nil
		}
		return os.ReadFile(path)
	}
	there := func(path string) error {
		if secretsLater {
			return nil
		}
		_, err := os.Stat(path)
		return err
	}
	var slackLater string
	// trustFile is --backtest-trust; backtestSet, that another
	// --backtest-* was given.
	var trustFile string
	var backtestSet bool
	takes := func(name, val string) (bool, error) {
		dur := func(min time.Duration) (time.Duration, error) {
			d, err := serve.ParseDuration(val)
			if err != nil || d < min {
				return 0, fmt.Errorf("%s %s: want a duration like 1h, 28d", name, val)
			}
			return d, nil
		}
		var err error
		switch name {
		case "--ledger":
			cfg.Ledger, ledgerSet = val, true
		case "--ledger-url-file":
			// Read once here, to say at once what is wrong; serve reads it
			// again, and keeps only the file's name.
			b, err := secret(val, 1)
			if err != nil {
				return true, fmt.Errorf("%s: %v", name, err)
			}
			if strings.TrimSpace(string(b)) == "" {
				return true, fmt.Errorf("%s: the file is empty: want a Postgres connection string", name)
			}
			cfg.LedgerURLFile = val
		case "--listen":
			cfg.Listen = val
		case "--allowed-hosts":
			cfg.AllowedHosts = list(val)
		case "--metrics-listen":
			cfg.MetricsListen = val
		case "--shutdown-delay":
			cfg.ShutdownDelay, err = dur(0)
			if err == nil && cfg.ShutdownDelay > 10*time.Minute {
				err = fmt.Errorf("--shutdown-delay %s: at most 10m", val)
			}
		case "--incidents-listen":
			cfg.IncidentsListen = val
		case "--responses-token-file":
			// SOURCE=FILE, once for each source: a token writes its source.
			source, file, ok := strings.Cut(val, "=")
			if !ok || file == "" {
				return true, fmt.Errorf("--responses-token-file %s: want SOURCE=FILE, a source and the file holding the token that writes its pages, like pagerduty-prod=/etc/lapilli/pd-token", val)
			}
			if !ledger.ValidSource(source) {
				return true, fmt.Errorf("--responses-token-file %s: a source is lowercase letters, digits and dashes, 40 at most, such as pagerduty-prod", source)
			}
			if source == ledger.ResponsesFile {
				return true, fmt.Errorf("--responses-token-file %s=: %s is the source --responses is read as, which owns its pages: name another", source, source)
			}
			if _, twice := cfg.ResponsesTokens[source]; twice {
				return true, fmt.Errorf("--responses-token-file %s given twice: one token writes a source", source)
			}
			b, err := secret(file, 16)
			if err != nil {
				return true, fmt.Errorf("%s: %v", name, err)
			}
			if len(strings.TrimSpace(string(b))) < 16 {
				return true, fmt.Errorf("%s: want a token of 16 characters at least", name)
			}
			if cfg.ResponsesTokens == nil {
				cfg.ResponsesTokens = map[string]string{}
			}
			cfg.ResponsesTokens[source] = file
		case "--incidents-token-file", "--firings-token-file", "--tasks-token-file", "--changes-token-file", "--changes-grafana-token-file",
			"--changes-ruler-token-file", "--changes-loki-token-file":
			b, err := secret(val, 16)
			if err != nil {
				return true, fmt.Errorf("%s: %v", name, err)
			}
			if len(strings.TrimSpace(string(b))) < 16 {
				return true, fmt.Errorf("%s: want a token of 16 characters at least", name)
			}
			switch name {
			case "--firings-token-file":
				cfg.FiringsToken = val
			case "--tasks-token-file":
				cfg.TasksToken = val
			case "--changes-token-file":
				cfg.ChangesToken = val
			case "--changes-grafana-token-file":
				cfg.ChangesGrafanaToken = val
			case "--changes-ruler-token-file":
				cfg.ChangesRulerToken = val
			case "--changes-loki-token-file":
				cfg.ChangesLokiToken = val
			default:
				cfg.IncidentsToken = val
			}
		case "--changes":
			cfg.Changes = val == "true"
		case "--approvers":
			cfg.Approvers = list(val)
		case "--approvals":
			if val != "1" && val != "2" {
				return true, fmt.Errorf("--approvals %s: want 1 or 2", val)
			}
			cfg.Approvals, _ = strconv.Atoi(val)
		case "--changes-limits":
			cfg.ChangesLimits, err = serve.ParseLimits(val)
			if err != nil {
				err = fmt.Errorf("--changes-limits %w", err)
			}
		case "--changes-paused":
			cfg.ChangesPaused = val
		case "--changes-in":
			for _, x := range list(val) {
				if !changesIn.MatchString(x) || len(x) > 1100 {
					return true, fmt.Errorf("--changes-in %s: want namespaces or rule groups, comma-separated, each a name or a prefix ending in *, like shop,team-*", x)
				}
				cfg.ChangesIn = append(cfg.ChangesIn, x)
			}
			if len(cfg.ChangesIn) == 0 {
				return true, fmt.Errorf("--changes-in %q: want namespaces or rule groups, comma-separated", val)
			}
		case "--changes-managed":
			k, v, ok := strings.Cut(val, "=")
			if !ok || strings.TrimSpace(k) == "" || v == "" || len(val) > 512 {
				return true, fmt.Errorf("--changes-managed %s: want a tag, KEY=VALUE or KEY=*", val)
			}
			cfg.ChangesManaged = append(cfg.ChangesManaged, val)
		case "--changes-aws-role":
			if !iamRole.MatchString(val) {
				return true, fmt.Errorf("--changes-aws-role %s: want a role's ARN, like arn:aws:iam::123456789012:role/lapilli-rules", val)
			}
			cfg.ChangesAWSRole = val
		case "--changes-azure-client-id":
			if !clientID.MatchString(val) {
				return true, fmt.Errorf("--changes-azure-client-id %s: want a client ID, like 00000000-0000-0000-0000-000000000000", val)
			}
			cfg.ChangesAzureClientID = val
		case "--changes-google-service-account":
			if !serviceAccount.MatchString(val) {
				return true, fmt.Errorf("--changes-google-service-account %s: want a service account's email, like lapilli-alarms@PROJECT.iam.gserviceaccount.com", val)
			}
			cfg.ChangesGoogleServiceAccount = val
		case "--changes-oci-config":
			if err := there(val); err != nil {
				return true, fmt.Errorf("--changes-oci-config: %v", err)
			}
			cfg.ChangesOCIConfig = val
		case "--mcp-propose":
			if val != "editors" && val != "admins" {
				return true, fmt.Errorf("--mcp-propose %s: want editors or admins", val)
			}
			cfg.MCPPropose = val
		case "--notify-listen":
			cfg.NotifyListen = val
		case "--notify-token-file", "--notify-grafana-secret-file":
			b, err := secret(val, 16)
			if err != nil {
				return true, fmt.Errorf("%s: %v", name, err)
			}
			if len(strings.TrimSpace(string(b))) < 16 {
				return true, fmt.Errorf("%s: want a secret of 16 characters at least", name)
			}
			if name == "--notify-token-file" {
				cfg.NotifyToken = val
			} else {
				cfg.NotifyGrafanaSecret = val
			}
		case "--notify-sns":
			for _, arn := range list(val) {
				if !snsTopic.MatchString(arn) {
					return true, fmt.Errorf("--notify-sns %s: want topic ARNs, like arn:aws:sns:eu-west-1:123456789012:alerts", arn)
				}
				cfg.NotifySNS = append(cfg.NotifySNS, arn)
			}
		case "--source-quiet":
			cfg.SourceQuiet, err = dur(time.Hour)
		case "--deploys":
			cfg.Deploys = val
		case "--marked":
			cfg.Marked = val
		case "--mark":
			marks = append(marks, val)
		case "--mark-match":
			markMatches = append(markMatches, val)
		case "--deploy-after", "--deploy-before":
			d, derr := dur(0)
			switch {
			case derr != nil:
				err = derr
			case d > ledger.MaxDeployAfter:
				err = fmt.Errorf("%s %s: a day at most", name, val)
			case name == "--deploy-after":
				deployAfter, deployTimes = d, true
			default:
				deployBefore, deployTimes = d, true
			}
		case "--runbook-base":
			if err = serve.CheckRunbookBase(val); err == nil {
				cfg.RunbookBase = val
			}
		case "--check-differs":
			n, ratio, ok := strings.Cut(val, "/")
			var d serve.Differs
			var e1, e2 error
			d.N, e1 = strconv.Atoi(n)
			d.Ratio, e2 = strconv.ParseFloat(ratio, 64)
			if !ok || e1 != nil || e2 != nil || d.N < 1 || d.Ratio <= 0 || d.Ratio > 1 {
				return true, fmt.Errorf("--check-differs %s: want N/RATIO, such as 5/0.9: at least N firings either way over a week, fewer than RATIO of them alike", val)
			}
			cfg.CheckDiffers = d
		case "--yield-for":
			cfg.YieldFor, err = dur(0)
			yieldForSet = true
			if err == nil && cfg.YieldFor > 24*time.Hour {
				err = fmt.Errorf("--yield-for %s: a day at most, well within the week a rule missed is replayed for", val)
			}
		case "--yield-on-429":
			cfg.Yield = val == "true"
		case "--guard":
			if strings.TrimSpace(val) == "" {
				return true, fmt.Errorf("--guard: want a PromQL expression, such as rate(prometheus_rule_evaluation_failures_total[5m]) > 0")
			}
			cfg.Guard = val
		case "--catch-up-hours":
			w, werr := serve.ParseWindow(val)
			if werr != nil {
				return true, fmt.Errorf("--catch-up-hours %w", werr)
			}
			cfg.CatchUpHours = &w
		case "--catch-up-rate":
			cfg.CatchUpRate, err = strconv.ParseFloat(val, 64)
			if err != nil || !(cfg.CatchUpRate >= prom.MinRate) || cfg.CatchUpRate > 10000 {
				return true, fmt.Errorf("--catch-up-rate %s: want requests a second, from %g, like 5 or 0.5", val, prom.MinRate)
			}
		case "--every":
			cfg.Every, err = dur(time.Minute)
		case "--settle":
			cfg.Settle, err = dur(0)
		case "--backfill":
			cfg.Backfill, err = dur(time.Hour)
			if err == nil && cfg.Backfill > serve.MaxBackfill {
				err = fmt.Errorf("--backfill %s: at most 3650d, ten years", val)
			}
		case "--keep":
			cfg.Keep, err = dur(0)
		case "--keep-activity":
			cfg.KeepActivity, err = dur(0)
			keepActivitySet = true
		case "--watch":
			cfg.Watch, err = dur(10 * time.Second)
		case "--alarms-every":
			cfg.AlarmsEvery, err = dur(time.Minute)
		case "--digest":
			cfg.Digest, err = serve.ParseWeekly(val)
			if err != nil {
				err = fmt.Errorf("--digest %w", err)
			}
		case "--user-header":
			cfg.UserHeader = val
		case "--read-only":
			cfg.ReadOnly = val == "true"
		case "--config":
			cfg.Config = val
		case "--config-prune":
			cfg.ConfigPrune = val == "true"
		case "--goal-tasks":
			cfg.GoalTasks = val == "true"
		case "--expect":
			cfg.Expect = val
		case "--expect-tasks":
			cfg.ExpectTasks = val == "true"
		case "--promtool":
			if cfg.Promtool, err = scenario.Resolve(val); err != nil {
				return true, fmt.Errorf("--promtool %s: %v", val, err)
			}
		case "--promtool-memory":
			if cfg.PromtoolMemory, err = memorySize(val); err != nil {
				return true, fmt.Errorf("--promtool-memory %s: %v", val, err)
			}
			promtoolMemorySet = true
		case "--proposals-expire":
			// 0, 0s or 0d: off.
			if d, perr := serve.ParseDuration(val); perr == nil && d == 0 {
				cfg.ProposalsExpire = 0
				break
			}
			cfg.ProposalsExpire, err = dur(time.Hour)
		case "--everyone-admin":
			cfg.EveryoneAdmin = val == "true"
		case "--editors":
			cfg.Editors = list(val)
		case "--admins":
			cfg.Admins = list(val)
		case "--groups-header":
			cfg.GroupsHeader = val
		case "--notify-webhook-file":
			if err := there(val); err != nil {
				return true, fmt.Errorf("--notify-webhook-file: %v", err)
			}
			cfg.NotifyWebhook = val
		case "--notify-webhook-secret-file":
			// Read once here, to say at once what is wrong; serve reads it
			// again for each post, and never shows it.
			b, err := secret(val, report.SigningKeyLeast)
			if err != nil {
				return true, fmt.Errorf("%s: %v", name, err)
			}
			if n := len(report.TrimKey(b)); n < report.SigningKeyLeast {
				return true, fmt.Errorf("%s: want a key of %d bytes at least, not %d", name, report.SigningKeyLeast, n)
			}
			cfg.NotifyWebhookSecret = val
		case "--notify-slack":
			cfg.NotifySlack = val == "true"
		case "--external-url":
			u, perr := url.Parse(val)
			if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return true, fmt.Errorf("--external-url %s: want where people reach the UI, like https://lapilli.example.com", val)
			}
			if u.Path != "" && u.Path != "/" {
				return true, fmt.Errorf("--external-url %s: lapilli is served at the root of its host, as its links are: give no path", val)
			}
			cfg.ExternalURL = val
			externalURLLocal = u.Scheme == "https" || thisMachine(u.Hostname())
		case "--oidc-issuer":
			if !signin.SafeURL(val) {
				return true, fmt.Errorf("--oidc-issuer %s: want the provider's https URL, as its ID tokens' iss has it (http only to this machine or a name inside a cluster)", val)
			}
			oc.Issuer, ocSet = val, true
		case "--oidc-client-id":
			if strings.TrimSpace(val) == "" {
				return true, fmt.Errorf("--oidc-client-id: want the client lapilli is registered as")
			}
			oc.ClientID, ocSet = val, true
		case "--oidc-client-secret-file":
			b, err := secret(val, 1)
			if err != nil {
				return true, fmt.Errorf("--oidc-client-secret-file: %v", err)
			}
			if strings.TrimSpace(string(b)) == "" {
				return true, fmt.Errorf("--oidc-client-secret-file: the file is empty")
			}
			oc.SecretFile, ocSet = val, true
		case "--oidc-scopes":
			oc.Scopes, ocSet, oidcScopesGiven = list(val), true, true
		case "--oidc-hosted-domain":
			for _, d := range list(val) {
				if !hostedDomain.MatchString(d) {
					return true, fmt.Errorf("--oidc-hosted-domain %s: want domains, like example.com", d)
				}
				oc.HostedDomains = append(oc.HostedDomains, strings.ToLower(d))
			}
			ocSet = true
		case "--oidc-trusted-clients":
			oc.TrustedClients, ocSet = list(val), true
		case "--oidc-bearer":
			oc.NoBearer, ocSet = val != "true", true
		case "--viewers":
			cfg.Viewers = list(val)
		case "--user-claim":
			if strings.TrimSpace(val) == "" {
				return true, fmt.Errorf("--user-claim: want a claim, such as email, preferred_username or sub")
			}
			claims.UserClaim, claimsSet = val, true
		case "--groups-claim":
			claims.GroupsClaim, claimsSet = strings.TrimSpace(val), true
		case "--session-max-age":
			d, derr := dur(5 * time.Minute)
			if derr != nil || d > 30*24*time.Hour {
				return true, fmt.Errorf("--session-max-age %s: want a duration from 5m to 30d", val)
			}
			cfg.SessionMaxAge, sessionSet = d, true
		case "--session-key-file":
			b, err := secret(val, 32)
			if err != nil {
				return true, fmt.Errorf("--session-key-file: %v", err)
			}
			if len(strings.TrimSpace(string(b))) < 32 {
				return true, fmt.Errorf("--session-key-file: want a key of 32 characters at least")
			}
			cfg.SessionKey, sessionSet = val, true
		case "--proxy-jwt":
			if !slices.Contains(signin.ProxyKinds, val) {
				return true, fmt.Errorf("--proxy-jwt %s: want %s", val, strings.Join(signin.ProxyKinds, ", "))
			}
			px.Kind, pxSet = val, true
		case "--proxy-jwt-audience":
			px.Audience, pxSet = val, true
		case "--proxy-jwt-alb-arn":
			px.ALBArn, pxSet = val, true
		case "--proxy-jwt-issuer":
			px.Issuer, pxSet = val, true
		case "--proxy-jwt-jwks":
			px.JWKS, pxSet = val, true
		case "--proxy-jwt-header":
			px.Header, pxSet = val, true
		case "--lang":
			if !slices.Contains(serve.Langs(), val) {
				return true, fmt.Errorf("--lang %s: want one of %s", val, strings.Join(serve.Langs(), ", "))
			}
			cfg.Lang = val
		case "--team-labels":
			cfg.TeamLabels = list(val)
			if len(cfg.TeamLabels) == 0 || len(cfg.TeamLabels) > 8 || slices.ContainsFunc(cfg.TeamLabels, func(k string) bool { return !labelName.MatchString(k) }) {
				return true, fmt.Errorf("--team-labels %s: want label names, comma-separated, eight at most, like team,owner", val)
			}
		case "--team-digest":
			team, file, ok := strings.Cut(val, "=")
			if !ok || team == "" || file == "" {
				return true, fmt.Errorf("--team-digest %s: want a team and a file, like payments=/run/secrets/payments", val)
			}
			if err := there(file); err != nil {
				return true, fmt.Errorf("--team-digest: %v", err)
			}
			if cfg.TeamDigests == nil {
				cfg.TeamDigests = map[string]string{}
			}
			cfg.TeamDigests[team] = file
		case "--report-dest":
			name, file, ok := strings.Cut(val, "=")
			if !ok || file == "" || !ledger.ValidDest(name) || name == "slack" || name == "webhook" || strings.HasPrefix(name, "team:") {
				return true, fmt.Errorf("--report-dest %s: want a name (lowercase letters, digits and . _ -, not slack, webhook or team:) and a file, like ops-daily=/run/secrets/ops-daily", val)
			}
			if err := there(file); err != nil {
				return true, fmt.Errorf("--report-dest: %v", err)
			}
			if cfg.ReportDests == nil {
				cfg.ReportDests = map[string]string{}
			}
			cfg.ReportDests[name] = file
		case "--webhook-dest":
			name, file, ok := strings.Cut(val, "=")
			if !ok || file == "" || !ledger.ValidDest(name) || name == "slack" || name == "webhook" || strings.HasPrefix(name, "team:") {
				return true, fmt.Errorf("--webhook-dest %s: want a name (lowercase letters, digits and . _ -, not slack, webhook or team:) and a file, like payments-hook=/run/secrets/payments-hook", val)
			}
			if err := there(file); err != nil {
				return true, fmt.Errorf("--webhook-dest: %v", err)
			}
			if cfg.WebhookDests == nil {
				cfg.WebhookDests = map[string]string{}
			}
			cfg.WebhookDests[name] = file
		case "--webhook-dest-secret":
			name, file, ok := strings.Cut(val, "=")
			if !ok || name == "" || file == "" {
				return true, fmt.Errorf("--webhook-dest-secret %s: want a --webhook-dest's name and a file, like payments-hook=/run/secrets/payments-hook-key", val)
			}
			// Read once here, as --notify-webhook-secret-file is: serve
			// reads it again for each post, and never shows it.
			b, err := secret(file, report.SigningKeyLeast)
			if err != nil {
				return true, fmt.Errorf("--webhook-dest-secret %s: %v", name, err)
			}
			if n := len(report.TrimKey(b)); n < report.SigningKeyLeast {
				return true, fmt.Errorf("--webhook-dest-secret %s: want a key of %d bytes at least, not %d", name, report.SigningKeyLeast, n)
			}
			if cfg.WebhookDestSecrets == nil {
				cfg.WebhookDestSecrets = map[string]string{}
			}
			cfg.WebhookDestSecrets[name] = file
		case "--backtest-listen":
			cfg.BacktestListen = val
		case "--backtest-trust":
			trustFile = val
		case "--backtest-share":
			cfg.BacktestShare, err = strconv.ParseFloat(val, 64)
			if err != nil || !(cfg.BacktestShare >= 0.05 && cfg.BacktestShare <= 0.75) {
				return true, fmt.Errorf("--backtest-share %s: want the lane's share of --max-rate, 0.05 to 0.75", val)
			}
			backtestSet = true
		case "--backtest-queue":
			cfg.BacktestQueue, err = strconv.Atoi(val)
			if err != nil || cfg.BacktestQueue < 1 || cfg.BacktestQueue > 1000 {
				return true, fmt.Errorf("--backtest-queue %s: want backtests waiting at most, 1 to 1000", val)
			}
			backtestSet = true
		case "--backtest-timeout":
			if cfg.BacktestTimeout, err = dur(time.Minute); err == nil && cfg.BacktestTimeout > 2*time.Hour {
				err = fmt.Errorf("--backtest-timeout %s: 2h at most", val)
			}
			backtestSet = true
		case "--mcp-compare":
			if val != "editors" && val != "admins" {
				return true, fmt.Errorf("--mcp-compare %s: want editors or admins", val)
			}
			cfg.MCPCompare = val
		case "--timeout":
			timeoutSet = true
			return false, nil
		case "--slack-webhook-file":
			if !secretsLater {
				return false, nil // verdict's, as for verdict
			}
			slackLater = val
		default:
			return false, nil
		}
		return true, err
	}
	f, err := parseFlags(args, takes)
	if err == nil && slackLater != "" {
		f.slackFile = slackLater
	}
	if !keepActivitySet {
		// Not given: 800 days, or --keep when that is longer, so that a
		// --keep past 800 days starts as it did before; with --keep 0,
		// nothing is pruned, and the activity log is kept whole.
		cfg.KeepActivity = max(cfg.KeepActivity, cfg.Keep)
		if cfg.Keep <= 0 {
			cfg.KeepActivity = 0
		}
	}
	signIns := 0
	for _, on := range []bool{cfg.UserHeader != "", ocSet, pxSet} {
		if on {
			signIns++
		}
	}
	signedIn := signIns > 0
	// What people did when a rule fired is read from a file, the history or
	// the deploys (docs/design/responses.md).
	usesResponses := f != nil && (f.responses != "" || f.rollouts) || cfg.Deploys != "" || len(cfg.ResponsesTokens) > 0
	switch {
	case err != nil:
	case ledgerSet && cfg.LedgerURLFile != "":
		err = fmt.Errorf("--ledger and --ledger-url-file are two places to keep the ledger: give one")
	case signIns > 1:
		err = fmt.Errorf("--user-header, --oidc-issuer and --proxy-jwt are three ways to say who people are: give one")
	case ocSet && (oc.Issuer == "" || oc.ClientID == ""):
		err = fmt.Errorf("--oidc-issuer and --oidc-client-id go together: the provider, and the client lapilli is registered as with it")
	case ocSet && cfg.ExternalURL == "":
		err = fmt.Errorf("--oidc-issuer needs --external-url: the provider sends people back to its /auth/callback")
	case ocSet && !externalURLLocal:
		err = fmt.Errorf("--external-url %s: with --oidc-issuer, want https, or http to this machine (localhost): the session cookie would otherwise go in the clear", cfg.ExternalURL)
	case pxSet && px.Kind == "":
		err = fmt.Errorf("--proxy-jwt-* go with --proxy-jwt: which sign-in in front signs the header")
	case pxSet && signin.CheckProxy(px) != nil:
		err = signin.CheckProxy(px)
	case claimsSet && !ocSet && !pxSet:
		err = fmt.Errorf("--user-claim and --groups-claim read the tokens of --oidc-issuer or --proxy-jwt")
	case sessionSet && !ocSet:
		err = fmt.Errorf("--session-max-age and --session-key-file are for --oidc-issuer's sessions")
	case cfg.GroupsHeader != "" && (ocSet || pxSet):
		err = fmt.Errorf("--groups-header is --user-header's: with --oidc-issuer or --proxy-jwt, the groups are in --groups-claim")
	case cfg.EveryoneAdmin && (ocSet || pxSet):
		err = fmt.Errorf("--everyone-admin is for no sign-in: to let everyone signed in change anything, give --admins '*'")
	case cfg.AlarmsEvery > 0 && len(f.alarms) == 0:
		err = fmt.Errorf("--alarms-every is how often --alarms is read: give --alarms")
	case cfg.Keep > 0 && cfg.Keep < cfg.Backfill:
		err = fmt.Errorf("--keep %s is shorter than --backfill %s", time.Duration(cfg.Keep), time.Duration(cfg.Backfill))
	case cfg.Keep > 0 && cfg.KeepActivity > 0 && cfg.KeepActivity < cfg.Keep:
		err = fmt.Errorf("--keep-activity %s is shorter than --keep %s: the activity log is kept as long as the firings at least", time.Duration(cfg.KeepActivity), time.Duration(cfg.Keep))
	case cfg.Keep <= 0 && cfg.KeepActivity > 0:
		err = fmt.Errorf("--keep-activity %s with --keep 0: nothing is pruned with --keep 0, the activity log neither: give --keep too, or leave --keep-activity out", time.Duration(cfg.KeepActivity))
	case cfg.EveryoneAdmin && (len(cfg.Editors) > 0 || len(cfg.Admins) > 0):
		err = fmt.Errorf("--everyone-admin is for no roles: with --editors or --admins, they say who may change what")
	case cfg.EveryoneAdmin && cfg.ReadOnly:
		err = fmt.Errorf("--everyone-admin lets everyone change anything, and --read-only no one")
	case (ocSet || pxSet) && serve.CheckRoles(serve.Config{OIDC: &oc, Viewers: cfg.Viewers, Editors: cfg.Editors, Admins: cfg.Admins, Approvers: cfg.Approvers}) != nil:
		err = serve.CheckRoles(serve.Config{OIDC: &oc, Viewers: cfg.Viewers, Editors: cfg.Editors, Admins: cfg.Admins, Approvers: cfg.Approvers})
	case len(cfg.Viewers) > 0 && !signedIn:
		err = fmt.Errorf("--viewers needs a sign-in, --user-header, --oidc-issuer or --proxy-jwt: who is who comes from it")
	case cfg.GroupsHeader != "" && cfg.UserHeader == "":
		err = fmt.Errorf("--groups-header needs --user-header: who is who comes from the proxy that signs people in")
	case (len(cfg.Editors) > 0 || len(cfg.Admins) > 0) && !signedIn:
		err = fmt.Errorf("--editors and --admins need a sign-in, --user-header, --oidc-issuer or --proxy-jwt: who is who comes from it")
	case cfg.NotifySlack && f.slackFile == "":
		err = fmt.Errorf("--notify-slack needs --slack-webhook-file")
	case cfg.NotifyWebhookSecret != "" && cfg.NotifyWebhook == "":
		err = fmt.Errorf("--notify-webhook-secret-file signs the posts to --notify-webhook-file: give it too")
	case len(cfg.WebhookDests) > 0 && cfg.Config == "":
		err = fmt.Errorf("--webhook-dest takes what a route in --config's notify: sends it: give --config")
	case webhookDestTwice(cfg) != "":
		err = fmt.Errorf("--webhook-dest %s: --report-dest gives a destination of that name already: give each its own", webhookDestTwice(cfg))
	case webhookSecretAlone(cfg) != "":
		err = fmt.Errorf("--webhook-dest-secret %s signs the posts to --webhook-dest %s: give it too", webhookSecretAlone(cfg), webhookSecretAlone(cfg))
	case cfg.IncidentsListen != "" && cfg.IncidentsToken == "" && cfg.FiringsToken == "" && cfg.TasksToken == "" && cfg.ChangesToken == "" && len(cfg.ResponsesTokens) == 0:
		err = fmt.Errorf("--incidents-listen takes what a token writes: add --incidents-token-file, --firings-token-file, --responses-token-file, --tasks-token-file, --changes-token-file or more than one")
	case cfg.IncidentsListen == "" && (cfg.IncidentsToken != "" || cfg.FiringsToken != "" || cfg.TasksToken != "" || cfg.ChangesToken != "" || len(cfg.ResponsesTokens) > 0):
		err = fmt.Errorf("--incidents-token-file, --firings-token-file, --responses-token-file, --tasks-token-file and --changes-token-file go with --incidents-listen, the port they are taken on")
	case responsesTokenClash(cfg) != "":
		err = errors.New(responsesTokenClash(cfg))
	case cfg.IncidentsToken != "" && cfg.FiringsToken != "" && sameFile(cfg.IncidentsToken, cfg.FiringsToken):
		err = fmt.Errorf("--incidents-token-file and --firings-token-file hold the same token: give each its own, so a job that may copy one can't write the other")
	case cfg.TasksToken != "" && (cfg.IncidentsToken != "" && sameFile(cfg.TasksToken, cfg.IncidentsToken) ||
		cfg.FiringsToken != "" && sameFile(cfg.TasksToken, cfg.FiringsToken) || cfg.NotifyToken != "" && sameFile(cfg.TasksToken, cfg.NotifyToken) ||
		cfg.ChangesToken != "" && sameFile(cfg.TasksToken, cfg.ChangesToken)):
		err = fmt.Errorf("--tasks-token-file holds the same token as another token file: give each its own, so a job keeping tasks in step can write nothing else")
	case cfg.ChangesToken != "" && (cfg.IncidentsToken != "" && sameFile(cfg.ChangesToken, cfg.IncidentsToken) ||
		cfg.FiringsToken != "" && sameFile(cfg.ChangesToken, cfg.FiringsToken) || cfg.NotifyToken != "" && sameFile(cfg.ChangesToken, cfg.NotifyToken)):
		err = fmt.Errorf("--changes-token-file holds the same token as another token file: give each its own")
	case cfg.Changes && !signedIn:
		err = fmt.Errorf("--changes needs a sign-in, --user-header, --oidc-issuer or --proxy-jwt: a change is approved by a person, and the sign-in says who")
	case cfg.Changes && len(cfg.Editors)+len(cfg.Admins) == 0:
		err = fmt.Errorf("--changes needs roles, --editors or --admins: without them, whoever may change anything is an admin, who could resume changes paused and approve")
	case cfg.Changes && len(cfg.Admins)+len(cfg.Approvers) == 0:
		err = fmt.Errorf("--changes needs --approvers or --admins: who approves a change, the admins unless --approvers says who")
	case cfg.Changes && cfg.ReadOnly:
		err = fmt.Errorf("--changes carries out changes, and --read-only refuses them")
	case cfg.Changes && f.rulesHistory != "" && len(f.rules) == 0 && len(f.alarms) == 0:
		// Followed, the rules are versions a recipe wrote, a chart's
		// rendering often: there is no file of rules to change.
		err = fmt.Errorf("--changes writes a change to where the rules are kept: followed alone, --rules-history is versions a recipe wrote, often rendered from a chart, with no file to write to. Give --rules, or leave --changes out")
	case !cfg.Changes && (len(cfg.Approvers) > 0 || cfg.Approvals > 0 || cfg.ChangesToken != "" || cfg.ChangesGrafanaToken != "" ||
		cfg.ChangesPaused != "" || cfg.ChangesLimits != (serve.Limits{}) || len(cfg.ChangesIn) > 0 || len(cfg.ChangesManaged) > 0 ||
		cfg.ChangesRulerToken != "" || cfg.ChangesLokiToken != "" || cfg.ChangesAWSRole != "" || cfg.ChangesAzureClientID != "" ||
		cfg.ChangesGoogleServiceAccount != "" || cfg.ChangesOCIConfig != ""):
		err = fmt.Errorf("--approvers, --approvals and the other --changes-* options go with --changes")
	case cfg.ChangesGrafanaToken != "" && f.grafana == "":
		err = fmt.Errorf("--changes-grafana-token-file is for the rules --grafana reads")
	case len(cfg.ChangesIn) > 0 && f.ruler == "" && len(f.alarms) == 0 && !f.lokiLoaded:
		err = fmt.Errorf("--changes-in is for the rules --ruler reads, those of Loki's ruler --loki-loaded-rules reads, and the alarms --alarms reads")
	case len(cfg.ChangesManaged) > 0 && f.ruler == "" && len(f.alarms) == 0:
		err = fmt.Errorf("--changes-in and --changes-managed are for the rules --ruler reads, and the alarms --alarms reads")
	case f.lokiLoaded && slices.ContainsFunc(f.alarms, func(a alarmsFlag) bool { return a.source == "loki" }):
		err = fmt.Errorf("--alarms loki=: with --loki-loaded-rules, loki: in --changes-in names Loki's ruler's namespaces: name the alarms' source another")
	case lokiChangesIn(cfg.ChangesIn, f) != "":
		err = fmt.Errorf("--changes-in %s names a namespace of Loki's ruler: give --loki URL and --loki-loaded-rules, which read it", lokiChangesIn(cfg.ChangesIn, f))
	case badChangesIn(cfg.ChangesIn, f) != "":
		err = fmt.Errorf("--changes-in %s: no --alarms names the source %s", badChangesIn(cfg.ChangesIn, f), strings.SplitN(badChangesIn(cfg.ChangesIn, f), ":", 2)[0])
	case cfg.ChangesRulerToken != "" && rulerKind(f) != source.RulerConfig:
		err = fmt.Errorf("--changes-ruler-token-file is for a Mimir's or a Cortex's rules, read with --ruler")
	case cfg.ChangesAWSRole != "" && rulerKind(f) != source.RulerAmazon && !alarmsOf(f, source.AlarmsAWS):
		err = fmt.Errorf("--changes-aws-role is for Amazon's rules, read with --ruler https://aps.REGION.amazonaws.com/workspaces/ID, or CloudWatch's alarms, read with --alarms")
	case cfg.ChangesAzureClientID != "" && rulerKind(f) != source.RulerAzure && !alarmsOf(f, source.AlarmsAzure):
		err = fmt.Errorf("--changes-azure-client-id is for Azure's rule groups, read with --ruler https://management.azure.com/subscriptions/SUB, or its alert rules, read with --alarms")
	case cfg.ChangesGoogleServiceAccount != "" && !alarmsOf(f, source.AlarmsGoogle):
		err = fmt.Errorf("--changes-google-service-account is for Google Cloud's alerting policies, read with --alarms")
	case cfg.ChangesOCIConfig != "" && !alarmsOf(f, source.AlarmsOCI):
		err = fmt.Errorf("--changes-oci-config is for OCI's alarms, read with --alarms")
	case len(cfg.ChangesManaged) > 0 && rulerKind(f) == source.RulerConfig && len(f.alarms) == 0:
		err = fmt.Errorf("--changes-managed names tags, which a Mimir's or a Cortex's groups don't have: --changes-in alone says what Lapilli may change there")
	case cfg.ChangesRulerToken != "" && f.rulerTk != "" && sameFile(cfg.ChangesRulerToken, f.rulerTk):
		err = fmt.Errorf("--changes-ruler-token-file holds the same token as --ruler-token-file: give the writes a token of their own")
	case cfg.ChangesLokiToken != "" && !f.lokiLoaded:
		err = fmt.Errorf("--changes-loki-token-file is for the rules of --loki's ruler, read with --loki-loaded-rules")
	case cfg.ChangesLokiToken != "" && f.lokiTk != "" && sameFile(cfg.ChangesLokiToken, f.lokiTk):
		err = fmt.Errorf("--changes-loki-token-file holds the same token as --loki-token-file: give the writes a token of their own")
	case cfg.ChangesLokiToken != "" && cfg.ChangesRulerToken != "" && sameFile(cfg.ChangesLokiToken, cfg.ChangesRulerToken):
		err = fmt.Errorf("--changes-loki-token-file holds the same token as --changes-ruler-token-file: give each ruler's writes a token of their own")
	case cfg.MCPPropose != "" && len(cfg.Editors)+len(cfg.Admins) == 0:
		err = fmt.Errorf("--mcp-propose needs --editors or --admins: without roles, whoever may change anything is an admin, and anyone who reaches /mcp could propose as anyone")
	case cfg.NotifyToken != "" && (cfg.IncidentsToken != "" && sameFile(cfg.NotifyToken, cfg.IncidentsToken) ||
		cfg.FiringsToken != "" && sameFile(cfg.NotifyToken, cfg.FiringsToken)):
		err = fmt.Errorf("--notify-token-file holds the same token as --incidents-token-file or --firings-token-file: give each its own, since alert managers keep the notifications token in webhook URLs more people can read")
	case cfg.IncidentsListen != "" && cfg.ReadOnly:
		err = fmt.Errorf("--incidents-listen takes writes, and --read-only refuses them")
	case cfg.NotifyListen != "" && cfg.NotifyToken == "" && cfg.NotifyGrafanaSecret == "" && len(cfg.NotifySNS) == 0:
		err = fmt.Errorf("--notify-listen needs --notify-token-file, --notify-grafana-secret-file or --notify-sns: what lets a notification in")
	case cfg.NotifyListen == "" && (cfg.NotifyToken != "" || cfg.NotifyGrafanaSecret != "" || len(cfg.NotifySNS) > 0):
		err = fmt.Errorf("--notify-token-file, --notify-grafana-secret-file and --notify-sns need --notify-listen")
	case cfg.NotifyListen != "" && cfg.ReadOnly:
		err = fmt.Errorf("--notify-listen takes writes, and --read-only refuses them")
	case f.noisyBy == verdict.NoisyByUseful && !usesResponses:
		err = fmt.Errorf("--noisy-by useful finds a rule noisy by what was done about its firings: give --responses, --responses-token-file, --rollouts or --deploys")
	case len(f.rolloutMatch) > 0 && !f.rollouts:
		err = fmt.Errorf("--rollout-match names the label that ties a firing to the workload it is about: it goes with --rollouts")
	case f.routine != 0 && !usesResponses:
		err = fmt.Errorf("--routine is the chance a change was routine: it goes with --responses, --responses-token-file, --rollouts or --deploys")
	case deployTimes && cfg.Deploys == "":
		err = fmt.Errorf("--deploy-before and --deploy-after go with --deploys, the file deploys are read from")
	case len(marks) > 0 && f.promURL == "" && f.gcpProj == "":
		err = errors.New("--mark is asked of the history: add " + historyAt)
	case cfg.MCPCompare != "" && len(cfg.Editors)+len(cfg.Admins) == 0:
		err = fmt.Errorf("--mcp-compare needs --editors or --admins: without roles, whoever may change anything is an admin, and anyone who reaches /mcp could run PromQL with lapilli's access")
	case cfg.ConfigPrune && cfg.Config == "":
		err = fmt.Errorf("--config-prune deletes what --config no longer declares: give --config")
	case cfg.GoalTasks && cfg.Config == "":
		err = fmt.Errorf("--goal-tasks makes tasks for the goals --config declares: give --config")
	case cfg.Config != "" && cfg.ReadOnly:
		err = fmt.Errorf("--config is kept in the ledger, and --read-only refuses writes")
	case cfg.ProposalsExpire > 0 && cfg.ReadOnly:
		err = fmt.Errorf("--proposals-expire closes proposals in the ledger, and --read-only refuses writes")
	case cfg.Config != "" && configFlag(cfg.Config) != nil:
		err = configFlag(cfg.Config)
	case cfg.Expect != "" && expectFlag(cfg.Expect) != nil:
		err = expectFlag(cfg.Expect)
	case cfg.Promtool != "" && cfg.Expect == "" && cfg.Config == "":
		err = fmt.Errorf("--promtool runs the scenarios of the expectations, --expect's or --config's: give one")
	case promtoolMemorySet && cfg.Promtool == "":
		err = fmt.Errorf("--promtool-memory is the memory --promtool's runs may take: give --promtool")
	case yieldForSet && !cfg.Yield:
		err = fmt.Errorf("--yield-for is how long --yield-on-429 leaves an API alone after a 429: give --yield-on-429")
	case cfg.Guard != "" && f.promURL == "" && f.gcpProj == "":
		err = errors.New("--guard is asked of the history: add " + historyAt)
	case cfg.CatchUpHours != nil && cfg.CatchUpHours.Len() < cfg.Every:
		err = fmt.Errorf("--catch-up-hours %s is shorter than --every %s: a run might never fall in it", cfg.CatchUpHours, time.Duration(cfg.Every))
	case cfg.BacktestListen != "" && trustFile == "":
		err = fmt.Errorf("--backtest-listen needs --backtest-trust: who may ask")
	case cfg.BacktestListen == "" && (trustFile != "" || backtestSet):
		err = fmt.Errorf("--backtest-trust, --backtest-share, --backtest-queue and --backtest-timeout go with --backtest-listen, the port backtests are taken on")
	case cfg.BacktestListen != "" && cfg.ReadOnly:
		err = fmt.Errorf("--backtest-listen queues backtests in the ledger, and --read-only refuses writes")
	case cfg.BacktestListen != "" && f.alerts:
		err = fmt.Errorf("--backtest-listen replays a pull request's rules, and --alerts judges the ALERTS series, not rules")
	case cfg.BacktestListen != "" && f.promURL == "" && f.gcpProj == "":
		err = errors.New("--backtest-listen replays a pull request's rules against the history: add " + historyAt)
	}
	var trustErr error
	if err == nil && trustFile != "" {
		// Every token and secret serve takes or sends: none may be a plain
		// entry's too.
		cfg.Trust, trustErr = serve.ReadTrust(trustFile, []string{cfg.IncidentsToken, cfg.FiringsToken, cfg.NotifyToken, cfg.TasksToken, cfg.ChangesToken,
			cfg.ChangesGrafanaToken, cfg.ChangesRulerToken, cfg.ChangesLokiToken, cfg.NotifyGrafanaSecret, f.grafanaTk, f.rulerTk, f.lokiTk, f.tokenFile}, secretsLater)
		if !trustSaid {
			err = trustErr
		}
	}
	if err == nil {
		cfg.Marks, err = serve.ParseMarks(marks, markMatches)
	}
	if err != nil {
		return serveSetup{}, err
	}
	if !timeoutSet {
		f.timeout = 50 * time.Minute
	}
	cfg.RulesPath = ""
	if len(f.rules) == 1 {
		cfg.RulesPath = f.rules[0].path
	}
	cfg.RulesHistory, cfg.RulesSelect = f.rulesHistory, f.rulesSelect
	cfg.Loaded, cfg.Grafana, cfg.GrafanaToken, cfg.Alerts = f.loadedRules, f.grafana, f.grafanaTk, f.alerts
	f.lokiInto(&cfg)
	if len(f.lokiRules) == 1 {
		cfg.LokiRules = f.lokiRules[0]
	}
	cfg.Ruler, cfg.RulerToken, cfg.RulerTenant, cfg.Auth = f.ruler, f.rulerTk, f.rulerTenant, f.auth
	for _, a := range f.alarms {
		cfg.Alarms = append(cfg.Alarms, serve.AlarmsAt{Source: a.source, URL: a.url})
	}
	cfg.RuleRE, cfg.Incidents, cfg.Impact, cfg.ImpactFor = f.ruleRE, f.incidents, f.impact, f.impactFor
	cfg.Options, cfg.Step, cfg.Loc, cfg.Slack = f.opts, f.step, f.loc, f.slackFile
	// What people did when a rule fired (docs/design/responses.md): the pages,
	// the changes read from the history, and the deploys; the Settings page
	// changes what rules are found noisy by, which this is the start of.
	cfg.Responses, cfg.Rollouts, cfg.RolloutMatch = f.responses, f.rollouts, f.rolloutMatch
	cfg.Options.Routine, cfg.Options.RolloutMatch = f.routine, f.rolloutMatch
	// Useful by default once pages or changes from the history are given;
	// --deploys alone, which serve took before responses, keeps caught.
	cfg.Options.NoisyBy = f.noisyBy
	if cfg.Options.NoisyBy == "" && f != nil && (f.responses != "" || f.rollouts || len(cfg.ResponsesTokens) > 0) {
		cfg.Options.NoisyBy = verdict.NoisyByUseful
	}
	// The named impact signals, which serve keeps in its ledger, each with
	// its own start: what judges by them is worked out from there.
	cfg.Signals, cfg.ImpactRules = f.signalDefs()
	cfg.Options.Signals = nil
	cfg.Options.DeployBefore, cfg.Options.DeployAfter, cfg.RunbookAnnotations = deployBefore, deployAfter, f.runbooks
	cfg.Late = f.late
	cfg.OnCall = f.onCall
	cfg.Timeout, cfg.Parallel = f.timeout, f.parallel
	cfg.CheckReplay, cfg.CheckSeries = checkReplayOn(f), f.checkSeries
	if f.historyGrafana() != "" && cfg.CheckReplay {
		h := f.grafanaHistory()
		cfg.History = &h
	}
	// The rules --grafana holds that can't be replayed, judged from its
	// own history (docs/design/grafanajudged.md).
	if f.grafana != "" && !f.noGrafanaHistory {
		h := f.grafanaHistory()
		h.URL = f.grafana
		cfg.JudgedHistory = &h
	}
	cfg.CheckContent, cfg.Severities, cfg.ScrapeInterval = checkContentOn(f), f.severities, f.scrapeInterval
	cfg.AlertmanagerConfig = f.amConfig
	if ocSet {
		if !oidcScopesGiven {
			oc.Scopes = []string{"email", "profile"}
		}
		oc.Claims = claims
		oc.Claims.Verified = true
		cfg.OIDC = &oc
	}
	if pxSet {
		px.Claims = claims
		cfg.Proxy = &px
	}
	return serveSetup{cfg: cfg, f: f, trustFile: trustFile, trustErr: trustErr}, nil
}

func runServe(args []string, stdout, stderr io.Writer) int { return serveOn(args, nil, stdout, stderr) }

// serveWith is what lapilli demo --serve gives lapilli serve that no flag
// says: the sample's history, in place of --prometheus's, which is then
// never asked; the clock serve judges by, pinned; and who is told where it
// listens, once bound. Its log says only warnings and errors.
type serveWith struct {
	history   history
	clock     func() time.Time
	listening func(net.Addr)
	// seed, when given, is serve's Config.Seed.
	seed func(ctx context.Context, s *serve.Server) bool
}

// serveOn is lapilli serve; with demo, as lapilli demo --serve runs it.
func serveOn(args []string, demo *serveWith, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdout, serveUsage)
			return 0
		}
	}
	st, err := readServe(args, false)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli serve: %s\nRun \"lapilli serve -h\" for the options.\n", verdict.Clean(err.Error()))
		return 2
	}
	cfg, f := st.cfg, st.f
	var level slog.Level
	if demo != nil {
		cfg.Clock, cfg.Listening, cfg.Seed, level = demo.clock, demo.listening, demo.seed, slog.LevelWarn
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	if f.checkGrafana != "" && f.grafanaTk != "" && plainHTTP(f.checkGrafana) {
		log.Warn("--check-grafana is plain http: the token goes unencrypted")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// With firings copied in alone, there may be no history to read.
	var h history
	if f.promURL != "" || f.gcpProj != "" {
		if demo != nil {
			h = demo.history
		} else if h, err = newHistory(ctx, f); err != nil {
			fmt.Fprintf(stderr, "lapilli serve: %s\n", verdict.Clean(err.Error()))
			return 1
		}
		h = f.withLogs(h, stderr)
	}
	s, err := serve.New(ctx, cfg, h, log)
	if err != nil {
		fmt.Fprintf(stderr, "lapilli serve: %s\n", verdict.Clean(err.Error()))
		return 1
	}
	defer s.Close()
	if cfg.Changes && cfg.Ruler != "" && !slices.ContainsFunc(cfg.ChangesIn, func(x string) bool { return !alarmsEntry(x, f) && !lokiEntry(x, f) }) {
		log.Warn("--changes with --ruler and no --changes-in: nothing in the ruler will be changed; its proposals are for a person")
	}
	if cfg.Changes && f.lokiLoaded && !slices.ContainsFunc(cfg.ChangesIn, func(x string) bool { return lokiEntry(x, f) }) {
		log.Warn("--changes with --loki-loaded-rules and no --changes-in loki:NAMESPACE: nothing in Loki's ruler will be changed; its proposals are for a person")
	}
	for _, a := range f.alarms {
		if cfg.Changes && !slices.ContainsFunc(cfg.ChangesIn, func(x string) bool { return strings.HasPrefix(x, a.source+":") }) {
			log.Warn("--changes with --alarms and no --changes-in for it: nothing in it will be changed; its proposals are for a person", "source", a.source)
		}
	}
	if err := s.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "lapilli serve: %s\n", verdict.Clean(err.Error()))
		return 1
	}
	return 0
}

// lokiInto gives cfg --loki, and the Limiter its queries are asked by,
// which lapilli serve reads its ruler by each minute too.
func (f *verdictFlags) lokiInto(cfg *serve.Config) {
	cfg.Loki, cfg.LokiToken, cfg.LokiTenant, cfg.LokiLoaded = f.loki, f.lokiTk, f.lokiTenant, f.lokiLoaded
	if f.loki != "" {
		cfg.LokiLimit = f.lokiSource().Limit
	}
}

// sameFile reports whether two paths name one file, or files holding the
// same token.
// responsesTokenClash says which of the --responses-token-file tokens holds
// the same token as another token file, another source's too, "" for none:
// each source's token writes its pages and nothing else.
func responsesTokenClash(cfg serve.Config) string {
	for _, source := range slices.Sorted(maps.Keys(cfg.ResponsesTokens)) {
		file := cfg.ResponsesTokens[source]
		for _, o := range []string{cfg.IncidentsToken, cfg.FiringsToken, cfg.TasksToken, cfg.NotifyToken, cfg.ChangesToken} {
			if o != "" && sameFile(file, o) {
				return "--responses-token-file " + source + " holds the same token as another token file: give each its own, so a job sending the pages of a paging tool can write nothing else"
			}
		}
		for other, f := range cfg.ResponsesTokens {
			if other > source && sameFile(file, f) {
				return "--responses-token-file " + source + " and " + other + " hold the same token: give each source its own, so a source can't write another's pages"
			}
		}
	}
	return ""
}

func sameFile(a, b string) bool {
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return strings.TrimSpace(string(x)) == strings.TrimSpace(string(y))
}

// rulerKind is the kind of ruler --ruler is, "" without one.
func rulerKind(f *verdictFlags) string {
	if f.ruler == "" {
		return ""
	}
	k, _ := source.RulerKind(f.ruler)
	return k
}

// changesIn is a namespace, or an Azure rule group (RG/NAME), as
// --changes-in names one: printable, without a comma, a * only at its end;
// or an alarm, SOURCE:NAME.
var changesIn = regexp.MustCompile(`^[^*,\x00-\x1f\x7f]+\*?$`)

// serviceAccount is a Google Cloud service account's email.
var serviceAccount = regexp.MustCompile(`^[a-z][-a-z0-9]{4,28}[a-z0-9]@[a-z0-9][-a-z0-9.:]{0,99}\.iam\.gserviceaccount\.com$`)

// alarmsOf reports whether an --alarms reads alarms of a cloud.
func alarmsOf(f *verdictFlags, kind string) bool {
	for _, a := range f.alarms {
		if k, _ := source.AlarmsKind(a.url); k == kind {
			return true
		}
	}
	return false
}

// alarmsEntry reports whether a --changes-in entry is an --alarms
// source's: SOURCE:NAME.
func alarmsEntry(x string, f *verdictFlags) bool {
	src, _, ok := strings.Cut(x, ":")
	return ok && slices.ContainsFunc(f.alarms, func(a alarmsFlag) bool { return a.source == src })
}

// lokiEntry reports whether a --changes-in entry names namespaces of
// --loki's ruler: loki:NAMESPACE, with --loki-loaded-rules. Without it,
// such an entry is --ruler's, as before.
func lokiEntry(x string, f *verdictFlags) bool {
	return f.lokiLoaded && strings.HasPrefix(x, "loki:")
}

// lokiChangesIn is a --changes-in entry, loki:NAMESPACE, given with Loki's
// rules from files alone or without --ruler, so that it can be no one's
// but a Loki ruler's that isn't read; "" for none.
func lokiChangesIn(in []string, f *verdictFlags) string {
	for _, x := range in {
		if strings.HasPrefix(x, "loki:") && !f.lokiLoaded && f.ruler == "" && !alarmsEntry(x, f) {
			return x
		}
	}
	return ""
}

// badChangesIn is a --changes-in entry that names a source no --alarms
// names, without a --ruler to be one of; "" for none.
func badChangesIn(in []string, f *verdictFlags) string {
	for _, x := range in {
		src, _, ok := strings.Cut(x, ":")
		if ok && ledger.ValidSource(src) && !alarmsEntry(x, f) && !lokiEntry(x, f) && f.ruler == "" {
			return x
		}
	}
	return ""
}

// iamRole is an IAM role's ARN, in any partition.
var iamRole = regexp.MustCompile(`^arn:aws(-cn|-us-gov)?:iam::[0-9]{12}:role/[\w+=,.@/-]{1,512}$`)

// clientID is a Microsoft Entra application's client ID.
var clientID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// snsTopic is an SNS topic's ARN, in any partition.
var snsTopic = regexp.MustCompile(`^arn:aws(-cn|-us-gov)?:sns:[a-z0-9-]+:[0-9]{12}:[A-Za-z0-9_-]{1,256}$`)

// labelName is a Prometheus label's name.
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`)

// list is a comma-separated list, without blanks.
func list(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// checkReplayOn says whether serve sets the replay beside what really
// fired: unless --check-replay=false.
func checkReplayOn(f *verdictFlags) bool { return !f.checkReplaySet || f.checkReplay }

// checkContentOn says whether serve checks what the rules say: unless
// --check-content=false, with rules to read.
func checkContentOn(f *verdictFlags) bool {
	return (!f.checkContentSet || f.checkContent) && !f.alerts && (len(f.rules) > 0 || readsAPI(f))
}

// hostedDomain is a domain, as Google's hd claim has one.
var hostedDomain = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$`)

// thisMachine reports whether a host is this machine's: localhost, a name
// under .localhost, or a loopback address, which a browser reaches without
// the network. A cookie set there over http still goes to every other
// server on this machine the browser visits, whatever its port (cookies
// aren't kept to a port): a name of its own under .localhost keeps it from
// the rest.
func thisMachine(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// configFlag refuses a --config file that isn't there, or can't be read: a
// flag given wrong. One that can't be used is said once serve runs, as when
// it changes to one: the ledger keeps what it last declared, and a pod
// restarted on it serves, rather than loop.
// expectFlag says an --expect file can't be read as one, at start: later,
// one that can't is said, and changes nothing.
func expectFlag(path string) error {
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		_, err = verdict.ReadExpectations(f)
	}
	if err != nil {
		return fmt.Errorf("--expect %s: %w", path, err)
	}
	return nil
}

// webhookDestTwice is a --webhook-dest's name --report-dest gives too, ""
// for none: a route names a destination by its name alone.
func webhookDestTwice(cfg serve.Config) string {
	for _, name := range slices.Sorted(maps.Keys(cfg.WebhookDests)) {
		if _, ok := cfg.ReportDests[name]; ok {
			return name
		}
	}
	return ""
}

// webhookSecretAlone is a --webhook-dest-secret's name no --webhook-dest
// gives, "" for none.
func webhookSecretAlone(cfg serve.Config) string {
	for _, name := range slices.Sorted(maps.Keys(cfg.WebhookDestSecrets)) {
		if _, ok := cfg.WebhookDests[name]; !ok {
			return name
		}
	}
	return ""
}

func configFlag(path string) error {
	if _, err := os.ReadFile(path); err != nil {
		return fmt.Errorf("--config: %w", err)
	}
	return nil
}

// defaultPromtoolMemory is --promtool-memory's default: a quarter of the
// chart's default memory limit (1Gi), so that a scenario run as an agent's
// is, beside serve's own, fits under it.
const defaultPromtoolMemory = 256 << 20

// memorySize reads a size of memory as Kubernetes writes one, a whole
// number of Mi or Gi, scenario.MinMemory at least.
func memorySize(v string) (uint64, error) {
	num, shift := v, 0
	switch {
	case strings.HasSuffix(v, "Mi"):
		num, shift = strings.TrimSuffix(v, "Mi"), 20
	case strings.HasSuffix(v, "Gi"):
		num, shift = strings.TrimSuffix(v, "Gi"), 30
	default:
		return 0, errors.New("want a size such as 256Mi or 1Gi")
	}
	n, err := strconv.ParseUint(num, 10, 32)
	if err != nil || num == "" || num[0] == '+' {
		return 0, errors.New("want a size such as 256Mi or 1Gi")
	}
	if n<<shift < scenario.MinMemory {
		return 0, fmt.Errorf("%dMi at least: promtool maps tens of MiB as it starts", scenario.MinMemory>>20)
	}
	return n << shift, nil
}
