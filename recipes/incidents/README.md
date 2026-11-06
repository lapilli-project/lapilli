# Incidents from where they are kept

A verdict is only as good as the incidents it is judged against, and these are
usually kept somewhere already. Each script here writes them to stdout in the
form `--incidents` takes:

| Script | Tool | Needs |
|---|---|---|
| `pagerduty.sh` | PagerDuty | `PAGERDUTY_TOKEN_FILE`; `SINCE`, `UNTIL` |
| `incident-io.sh` | incident.io | `INCIDENT_IO_TOKEN_FILE`; `SINCE` |
| `jira.sh` | Jira Cloud, Jira Service Management | `JIRA_URL`, `JIRA_EMAIL`, `JIRA_TOKEN_FILE`, `JIRA_JQL` |
| `github.sh` | GitHub issues | `gh`, signed in; `GITHUB_REPO`; `GITHUB_LABEL`, `LIMIT` |

They need `sh`, `curl` and `jq`. Tokens are read from files and passed to `curl`
on stdin, so `ps` doesn't show them. Give them read-only access.

Each tool's script is a few lines of paging; what it means by an incident is in
the `.jq` file beside it, for you to change:

- **Who counts.** PagerDuty's low-urgency incidents, and incident.io's tests,
  tutorials, and those in triage, declined, merged or canceled, are left out.
- **When it started.** PagerDuty's and Jira's times are when the incident was
  opened, which can be later than when it began. incident.io's `Impact started`
  is used when there is one.
- **Where.** A service becomes the label `service`, for `--match service`:
  PagerDuty's service, Jira's components, incident.io's custom field
  `Affected services`, and GitHub's labels like `service:checkout`. Rename them
  to what your alerts' labels say.

These are tested against each API's published schema with a server standing in
for it (`internal/serve/recipes_test.go`), not against real accounts. If one
doesn't work with yours, please say so.

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
of history would go one line at a time. One changed in the tool is changed in the
ledger, whatever someone wrote there meanwhile. One taken away in the tool
stays, until it is deleted with `DELETE /api/v1/incidents/{id}`. An incident over
31 days long is refused; `put.sh` says which, writes the rest and exits non-zero.

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
  jobTemplate:
    spec:
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
digit. Times are RFC 3339. `end` and `labels` are optional. Please send it here.
