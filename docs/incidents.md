# Incidents kept elsewhere

The thinner the incident log, the worse every verdict, so copy it from where
it is kept. [recipes/incidents](../recipes/incidents) has a small script for each
of PagerDuty, incident.io, Jira, GitHub issues, Grafana IRM, Rootly,
FireHydrant and Compass's alerts (for Opsgenie's users, whose incidents move to
Jira Service Management, which the Jira one reads). Each writes what
`--incidents` takes, so it works with both commands:

```sh
PAGERDUTY_TOKEN_FILE=pd-token SINCE=2026-10-01 recipes/incidents/pagerduty.sh > incidents.tmp &&
  mv incidents.tmp incidents.yaml
lapilli verdict --rules alert-rules.yaml --prometheus http://prometheus:9090 --incidents incidents.yaml
```

Move the file into place: an incidents file with nothing in it is refused, so
that a script that failed doesn't read as a week with no incidents. Write
`incidents: []` for that.

## The alerts a tool linked

An incident tool often knows which alerts an incident came from: PagerDuty
groups them under it, incident.io routes them to it, Rootly lists them. The
PagerDuty, incident.io and Rootly scripts write those as the incident's
`alerts`, each the rule it is of, by its `alertname`, and its labels:

```yaml
incidents:
  - id: PD-101
    start: 2026-10-28T12:01:00+09:00
    labels: {service: [checkout]}
    alerts:
      - {rule: PaymentsDBConnections, labels: {service: payments-db}}
```

A firing of one catches the incident when it starts between `--lead` before
it and its end, its labels agreeing on `--match` or not: the tool's link is
better evidence than a label. The report marks the catch `linked`. With
`--linked-only`, an incident that lists alerts is caught by those alone, and
one that lists none as before ([How it judges](how-it-judges.md)).

An alert carries its rule and labels only where the tool kept them, and
the scripts write only those whose alert carries an `alertname`, saying on
stderr how many they left out:

- **PagerDuty** keeps what Alertmanager puts in an alert's `details`. An
  alert is one Alertmanager sent for a group of alerts, and from
  Alertmanager 0.30.0 its details list each of them, with its labels, in
  `firing` and `resolved` by default: each is one of the incident's
  alerts. Before 0.30.0 they are text: add the rule and the labels you
  match on, and the alert is the group's, with the labels all its alerts
  have:

  ```yaml
  receivers:
    - name: pagerduty
      pagerduty_configs:
        - routing_key_file: /etc/alertmanager/pagerduty-key
          details:
            alertname: '{{ .CommonLabels.alertname }}'
            service: '{{ .CommonLabels.service }}'
  ```

- **incident.io** keeps an alert's labels as the attributes its alert
  source takes from the payload: give it one named `alertname`, and one for
  each label you match on, named as the label.
- **Rootly** keeps `alertname`, `instance`, `dc` and `job` of an alert
  Alertmanager sent among its labels.

`ALERT_LABELS=service,namespace` keeps those labels of each alert alone. Keep
to labels the rules' own firings carry: a label Prometheus adds as it sends
an alert (an external label such as `cluster`), or one only the tool has,
keeps every firing from matching. `ALERTS=no` reads no alerts. Reading them
is a request or more an incident, `MAX_ALERT_REQUESTS` (2000) a run at most;
at Rootly's five requests a minute, set `SINCE` close.

`lapilli serve` can also take them as they come. With `--incidents-listen ADDR`
and `--incidents-token-file FILE`, it opens a port that takes incidents from
whoever holds the token, and does nothing else. A job can run this every hour:
an incident written again as it was changes nothing, tells no one, and judges
nothing again. Unlike a file of the last month, it forgets nothing older.

```sh
recipes/incidents/pagerduty.sh > incidents.json &&
  LAPILLI_TOKEN_FILE=token recipes/incidents/put.sh http://lapilli:8082 < incidents.json
```

The port is its own so that a NetworkPolicy can let the job reach it alone. On
the UI's port a pod could name itself anyone, in the headers the sign-in proxy
sets; here those headers mean nothing. What the job copies in is the tool's: the
UI can't change it, and the token can't change what people wrote. It isn't
counted as anyone's use of Lapilli, and isn't told to the notice webhook, since the
tool has told people already. The port takes one write at a time, 20 a second,
and the ledger keeps 20,000 copied incidents at most.
