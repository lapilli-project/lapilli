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
