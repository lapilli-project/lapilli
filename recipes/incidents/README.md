# Incidents from where they are kept

A verdict is only as good as the incidents it is judged against, and these are
usually kept somewhere already. Each script here writes them to stdout in the
form `--incidents` takes:

| Script | Tool | Needs |
|---|---|---|
| `pagerduty.sh` | PagerDuty | `PAGERDUTY_TOKEN_FILE`; `SINCE`, `UNTIL` |
| `incident-io.sh` | incident.io | `INCIDENT_IO_TOKEN_FILE`; `SINCE` |
| `jira.sh` | Jira Cloud, Jira Service Management | `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN_FILE`, `JIRA_JQL`; `JIRA_START_FIELD`, `JIRA_END_FIELD`, `JIRA_LABELS`, `JSM_CLOUD_ID` |
| `github.sh` | GitHub issues | `gh`, signed in; `GITHUB_REPO`; `GITHUB_LABEL`, `LIMIT` |
| `compass.sh` | Compass alerts | `COMPASS_CLOUD_ID`, `COMPASS_EMAIL`, `COMPASS_TOKEN_FILE`; `SINCE`, `COMPASS_QUERY`, `MAX_LOGS` |
| `grafana-irm.sh` | Grafana IRM | `GRAFANA_URL`, `GRAFANA_IRM_TOKEN_FILE`; `SINCE` |
| `rootly.sh` | Rootly | `ROOTLY_TOKEN_FILE`; `SINCE` |
| `firehydrant.sh` | FireHydrant | `FIREHYDRANT_TOKEN_FILE`; `SINCE` |

They need `sh`, `curl` and `jq`. Tokens are read from files and passed to `curl`
on stdin, so `ps` doesn't show them. Give them read-only access. A token file
holds the token alone, on one line: PagerDuty's, incident.io's, Grafana IRM's,
Rootly's and FireHydrant's, which go in a header as they are, of the
characters a Bearer token is written in (letters, digits and `- . _ ~ + / =`);
Jira's and Compass's, which Atlassian says nothing of and which go base64'd
with the email, with no space. The address a token goes to (`JIRA_URL`,
`GRAFANA_URL`, or the API a script asks, which `PAGERDUTY_API` and the like
change) is `https://`, or this machine's over `http://` for tests, and `curl`
speaks no other scheme; one with a user or token before `@` is refused.
Neither the token nor the address is said.

`pagerduty.sh`, `incident-io.sh`, `rootly.sh`, `firehydrant.sh` and, for
Jira Service Management's incidents given `JSM_CLOUD_ID`, `jira.sh` also
write the alerts the tool linked to each incident, where an alert carries
its `alertname` and labels
([Incidents kept elsewhere](../../docs/incidents.md), "The alerts a tool
linked"). `alerts.jq` is what they share; give `jq` `-L` this directory to run
their `.jq` files yourself. Each alert keeps every label it carries: in
judging, one the firing doesn't carry (an external label Prometheus added)
is passed over, and one both carry must agree. A PagerDuty alert from an
Alertmanager before 0.30.0, whose details say `alertname` beside text that
may be anything, is its rule alone, with the labels `ALERT_LABELS` names
from its details. `ALERTS=no` reads none,
`ALERT_LABELS` keeps the labels it names alone, to narrow them, and
`MAX_ALERT_REQUESTS` (2000) is the most they ask for a run, past which they
stop and write nothing.

Each tool's script is a few lines of paging; what it means by an incident is in
the `.jq` file beside it, for you to change:

- **Who counts.** PagerDuty's low-urgency incidents, and incident.io's tests,
  tutorials, and those in triage, declined, merged or canceled, are left out;
  so are Grafana IRM's drills, Rootly's tests, examples, scheduled
  maintenance and incidents in triage or cancelled, and FireHydrant's
  archived incidents. Compass's alerts of priority P1 and P2 count.
- **When it started.** PagerDuty's times are when the incident was opened,
  which can be later than when it began, and so are Jira's unless
  `JIRA_START_FIELD` names the field yours keep it in (below).
  incident.io's `Impact started` is used when there is one, and so are
  Grafana IRM's incident start, Rootly's `started_at` and FireHydrant's
  `started_at`. An incident ends when Grafana IRM's incident end says, or it
  was closed; at Rootly's resolved, mitigated or closed; at FireHydrant's
  resolved milestone, or else its mitigated one.
- **Where.** A service becomes the label `service`, for `--match service`:
  PagerDuty's service, Jira's components or the fields `JIRA_LABELS`
  names, incident.io's custom field `Affected services`, GitHub's labels
  like `service:checkout`, Grafana IRM's labels whose key is `service`,
  Rootly's and FireHydrant's services, and the `service` an alert in Compass
  carries among its extra properties, as Alertmanager sends its labels.
  Rename them to what your alerts' labels say.

### Jira's fields

Teams often keep an incident's times and services in fields of their own.
An issue is created when someone gets to it, hours or days after the
incident began; its resolution date can be empty though its status says
resolved; components go unused. Three settings say where `jira.sh` looks:

- `JIRA_START_FIELD`: the field an incident starts at, `created` by default.
  A custom field is named by its id, such as `customfield_10001`. Several,
  comma-separated, are tried in turn, and the first holding a time is the
  start: with `customfield_10001,created`, an issue whose field is empty
  starts when it was created.
- `JIRA_END_FIELD`: the same for its end, `resolutiondate` by default. An
  issue none of them gives an end to has none, and is taken to last an hour.
- `JIRA_LABELS`: labels from fields, `service=components` by default, such
  as `service=components,team=customfield_10004`; 32 labels at most. Two
  fields can give the same label. A field gives what Jira gives: a text, a
  number, a list of them, an option's value (each of a multi-select's, and
  a cascading select's parent), a component's name and a user's display
  name. Empty ones are left out. Empty, there are no labels.

`GET /rest/api/3/field` lists the fields' ids and names. One whose
`schema.type` is `date` holds a day and no time, such as `2026-11-05`; one
whose type is `datetime` holds a time.

**A day is never taken for a time.** As midnight, or as the whole day, it
would have every firing that day catch the incident. A time is RFC 3339, as
Jira writes a date-time field's, with a real day, an hour to 23 and a zone
to 23:59; `2026-02-30T10:00:00Z` and `2026-11-14T10:00:60+09:00` aren't
times. A date, alone or with something after it that isn't a time (as a
text field's `2026-11-05 10:30`), still says the day. So in the list, a
later field's time is taken only if it is on that day, as Jira writes the
time: in the time zone of the profile of the account the script signs in
as. With `customfield_10001,created`, an issue created that day starts when
it was created, and one created on another day doesn't, since `created`
would start it late by days. A field holding anything else ends the list
there. The end's list works the same way.

What can't be read:

- **A start**, or none at all: the issue is skipped.
- **An end**, or one before the start: the incident is kept with no end,
  and taken to last an hour.
- **A label value** serve would refuse the incident for: over 256 bytes,
  past 64 a label, past 4 KB of labels in all. It is left out, and the
  incident kept.

Each is said on stderr by the issue's key, never its title, with a line
counting them by why:

```
jira.sh: "OPS-52" skipped: the start field customfield_10001 holds a date and no time
jira.sh: "OPS-56" kept with no end, taken to last an hour: the end field customfield_10002 holds a date and no time
jira.sh: skipped 3 of 40 issues: no start in customfield_10001 (1); the start field customfield_10001 holds a date and no time (2)
jira.sh: kept 1 of 40 issues with no end: the end field customfield_10002 holds a date and no time (1)
```

When every issue the search found is skipped, the script writes nothing
and exits 1, so a cron job fails: a field holding dates only, or one named
wrong, shows there instead of as no incidents. A search that finds nothing
writes none, and succeeds. If your team fills the start field in later, a
run whose issues are all still unfilled fails too, and says `no start in`
the field for each: nothing is wrong but the wait. Add `created` after the
field, so an issue starts when it was created until the field is filled
in and then moves, or narrow `JIRA_JQL` to issues with the field set
(`AND cf[10001] is not EMPTY`), so a run with none of them finds nothing
and succeeds.

The ids are the issues' keys, `OPS-41`, whatever the settings: change
them, and the incidents copied in before are the same incidents.

### Jira Service Management's linked alerts

With `JSM_CLOUD_ID`, the site's cloud id (as `compass.sh` takes it), each
issue's linked alerts are read as the same account, from Jira Service
Management's incident API, which Atlassian says is **in beta** and may
change: `GET /jsm/incidents/cloudId/{cloudId}/v1/incident/{issueID}/alert`
lists their ids, a page at a time by its `startKey`, and the operations
API's `GET /jsm/ops/api/{cloudId}/v1/alerts/{id}` gives each, one request
an alert. An API token with scopes needs `read:incident:jira-service-management`
and `read:ops-alert:jira-service-management` besides Jira's.

Alertmanager sends Jira Service Management its alerts through its
Opsgenie notifier (`opsgenie_configs`, as Atlassian's Prometheus
integration says), an alert a group, and from 0.22.0 puts the labels the
group's alerts share in its `details`, which the alert keeps as its extra
properties. One with `alertname` among them is of that rule, with the
others as its labels; the `details` you set are among them too, so
`ALERT_LABELS` narrows them to the labels you match on. Group by
`alertname`, or alerts of two rules share none and are left out.

An issue the incident API answers 400 for isn't an incident, as it says,
and has no alerts read; an alert the operations API no longer has (404)
is left out. Each is counted on stderr. Anything else it answers, and a
page of alerts it gave before, stops the script with nothing written; a
429 is waited out as its `Retry-After`
says, two minutes at most, five times a request and ten minutes a run.
`ALERTS=no` reads none.

### Compass, and Opsgenie's users

Opsgenie's support ends on 5 April 2027. Its incidents, with their linked
alerts and post-incident reviews, move to Jira Service Management, where
`jira.sh` reads them: `JIRA_JQL='project = "Opsgenie Migrated Incidents" AND
issuetype = Incident'`, or the service project yours went to, and with
`JSM_CLOUD_ID` their linked alerts. Its alerting and on-call move to Jira
Service Management or to Compass.

Compass keeps alerts, not incidents, so `compass.sh` takes Compass's alerts
through its operations API (`/compass/cloud/{cloudId}/ops/v1/alerts`, basic
auth with an Atlassian account's email and API token; `COMPASS_CLOUD_ID` is
the site's cloud id, not the organisation's). Which alerts are incidents is
yours to say, in `compass.jq` or with `COMPASS_QUERY`, Compass's alert
search: by default those of priority P1 and P2. An alert that the rules
Lapilli judges raised would only catch itself, so leave those out by source
or tag. Compass's alerts don't say when they were closed; their logs
(`/v1/alerts/{id}/logs`) do, so a closed alert that counts ends at the last
entry of its log saying it was closed (`compass-closed.jq`: Compass doesn't
document the words, so an entry of type `close`, or starting `Alert
closed`, as Opsgenie's do), five pages of its log at most, and at its last
update where its log says none or wasn't read: past `MAX_LOGS` (500)
alerts a run, which the script says. It answers 20,000 alerts of a search
at most: a longer history is refused, so set `SINCE`.

These four were written from each tool's API reference: Compass's
operations API, Grafana IRM's incident API (`IncidentsService.QueryIncidents`,
a service account token), Rootly's `GET /v1/incidents` and FireHydrant's
`GET /v1/incidents` (a bot token). Where a reference shows a shape two ways,
both are read. Rootly takes five requests a minute, and FireHydrant 50 an
account every 10 seconds: a 429 is waited out as its `Retry-After` says (or
else a minute for Rootly, and 5 seconds, then twice as long each time, for
FireHydrant), two minutes at most a wait, five waits a request and ten
minutes a run. Rootly and FireHydrant number
their pages with no order of their own, so an incident on two pages is one.

FireHydrant's alerts are those `GET /v1/incidents/{incident_id}/alerts`
lists for an incident. Its reference says only that an alert's `labels`
are "arbitrary key:value pairs"; its Alertmanager source names the labels
a group's alerts share `alertmanager/labels-NAME` on the event an alert
comes from. Both are read, the prefix taken off, so an alert with
`alertname` among them is of that rule. The reference gives the list
pages but no way to ask for the next: it is asked for with `page`, as the
incidents' are.

These are tested against each API's published schema with a server standing in
for it (`internal/serve/recipes_test.go`), not against real accounts. If one
doesn't work with yours, please say so. A `SINCE` that isn't a date or an RFC
3339 time is refused before anything is asked of the tool.

## Once, or from cron

```sh
PAGERDUTY_TOKEN_FILE=pd-token SINCE=2026-10-01 ./pagerduty.sh > incidents.tmp &&
  mv incidents.tmp incidents.yaml
lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --incidents incidents.yaml
```

Write to another file and move it: the shell empties a file before a script
writing to it has started, and a script that fails leaves it empty. An empty
file is refused, so a verdict stops there instead of finding no incidents.

`lapilli serve --incidents FILE` reads the file again each run, and the file owns
what is in it: the UI can't change those incidents, and taking one out of the
file takes it out of the ledger. So a file of the last month would forget older
incidents, every run. Give it all you want kept, or use the port below, where an
incident stays until it is deleted.

## Every hour, into lapilli serve

Start `lapilli serve` with a port that takes incidents and nothing else:

```sh
lapilli serve ... --incidents-listen :8082 --incidents-token-file /run/secrets/lapilli-incidents
```

Then, from anywhere that can reach it, by a name it answers to (`lapilli` or
`lapilli.monitoring.svc` in a cluster; others go in `--allowed-hosts`):

```sh
./pagerduty.sh > incidents.json &&
  LAPILLI_TOKEN_FILE=/run/secrets/lapilli-incidents ./put.sh http://lapilli:8082 < incidents.json
```

Give `put.sh` a file: in a pipe, a shell doesn't see the first command fail, and
the job would look fine with nothing copied.

Each incident is written by its id, so running this again is safe: one that
hasn't changed writes nothing. New ones aren't told to `--notify-webhook-file` or
Slack either, since the tool has told people already, and a first copy of months
of history would go one line at a time.

`lapilli serve`'s Settings page lists each source under Incident sources, by
its ids' prefix (`PD-` here, `GH-`, `IRM-`, a Jira project's key), with when
it last wrote, changed or not, and how many incidents it gave in four weeks:
quiet after two days with nothing, so a job that stopped shows. Give a tool
of your own a prefix of its own (`OPS-41`, not `41`).

What the token copies in is the tool's, as the incidents file's are the file's:
it is changed there, and the UI can't change it. The token can change only what
it copied in, so an id a person used already is refused, and so is one from the
incidents file; and the incidents file takes an id a copy got to first. One changed in the tool is changed in the ledger. One taken away
in the tool stays, until it is deleted with `DELETE /api/v1/incidents/{id}`. An
incident over 31 days long is refused; `put.sh` says which, writes the rest and
exits non-zero.

The port takes one write at a time, 20 a second, with bursts of 40; past that it
answers 429, and `put.sh` waits and tries again. The ledger keeps 20,000 copied
incidents at most. A script stops after `MAX_PAGES` pages
(1000), when the tool gives the same page again, or at an answer over 16 MiB,
and then writes nothing.

In the chart:

```yaml
serve:
  incidentsAPI:
    tokenSecret: {name: lapilli-incidents, key: token}
    from:
      - podSelector: {matchLabels: {app: lapilli-incidents-sync}}
```

and a CronJob of your own, labelled `app: lapilli-incidents-sync`, with the scripts
in a ConfigMap (`kubectl create configmap lapilli-recipes --from-file=recipes/incidents`)
and an image that has `sh`, `curl` and `jq`:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: lapilli-incidents-sync
spec:
  schedule: "*/30 * * * *"
  concurrencyPolicy: Forbid
  # A run that hangs shouldn't keep every later one from starting.
  startingDeadlineSeconds: 600
  jobTemplate:
    spec:
      activeDeadlineSeconds: 1200
      template:
        metadata:
          labels: {app: lapilli-incidents-sync}
        spec:
          restartPolicy: Never
          containers:
            - name: sync
              image: YOUR-IMAGE-WITH-SH-CURL-AND-JQ
              command: [sh, -c, 'f=$(mktemp) && sh /recipes/pagerduty.sh > "$f" && sh /recipes/put.sh http://lapilli:8082 < "$f"']
              env:
                - {name: PAGERDUTY_TOKEN_FILE, value: /secrets/pagerduty/token}
                - {name: LAPILLI_TOKEN_FILE, value: /secrets/lapilli/token}
              volumeMounts:
                - {name: recipes, mountPath: /recipes}
                - {name: pagerduty, mountPath: /secrets/pagerduty}
                - {name: lapilli, mountPath: /secrets/lapilli}
          volumes:
            - {name: recipes, configMap: {name: lapilli-recipes}}
            - {name: pagerduty, secret: {secretName: pagerduty-read-token}}
            - {name: lapilli, secret: {secretName: lapilli-incidents}}
```

The port is its own so that a NetworkPolicy can let the job reach it and nothing
else. On the UI's port a pod could name itself anyone, in the headers the sign-in
proxy sets.

## Another tool

Write a script that gives this, as JSON, and it works with `--incidents` and
`put.sh`:

```json
{"incidents": [
  {"id": "OPS-41", "title": "Checkout errors", "start": "2026-10-28T12:01:00+09:00",
   "end": "2026-10-28T12:40:00+09:00", "labels": {"service": ["checkout"]}}
]}
```

An id is letters, digits and `. _ : / # -`, up to 64, starting with a letter or
digit. Times are RFC 3339. `end`, `labels` and `alerts` are optional: `alerts`
are those the tool linked to it, each `{"rule": "CheckoutErrors", "labels":
{"service": "checkout"}}`, 100 at most. Please send it here.
