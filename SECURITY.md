# Security

Report a vulnerability privately through
[GitHub Security Advisories](https://github.com/lapilli-project/lapilli/security/advisories/new),
not in a public issue.

`lapilli verdict` reads rule, incident and firing files, and queries the
Prometheus-compatible API it is given, with the token it is given or, with
`--auth`, the AWS, Azure or Google Cloud credentials the machine already
has. With `--grafana` it reads the alert rules a Grafana holds, with the
token in `--grafana-token-file`; with `--ruler`, those a Mimir or Cortex
ruler, Amazon Managed Service for Prometheus or Azure's Prometheus rule
groups hold, with `--ruler-token-file` or the cloud's credentials; with
`--alarms`, a cloud's own alarms (CloudWatch, Azure Monitor, Google Cloud
Monitoring, OCI Monitoring), with that cloud's credentials. It sends
its report only where it is told
to: a Pushgateway (`--push`) and a Slack webhook (`--slack-webhook-file`). Of
particular interest:

- a way to make it send a query, a token, a credential or a signature
  anywhere other than the API, the Grafana, the ruler or the cloud it was
  given (a redirect, an Azure `nextLink` elsewhere, a name or an ID read
  from a ruler or a cloud that escapes its path), or the report
  anywhere other than where it was told;
- a way to make it show a Slack webhook's URL or a token, or a report make
  Slack notify people (a mention, `<!channel>`);
- a way for a rule file, an incident file, a firing file or an API answer to
  make it write a file, run a command, or send escape sequences to your
  terminal;
- a rule or answer that makes it use memory or time far out of proportion.

`lapilli serve` does the same every hour, and serves a web UI and API that
read and write its ledger (one SQLite file). It has no sign-in of its own:
it listens on localhost unless told otherwise, trusts the headers named by
`--user-header` and `--groups-header` for who is asking, gives them the role
`--editors` and `--admins` do, refuses posts from other sites, and with
`--read-only` takes no changes. It sends notices to the URLs in
`--notify-webhook-file` and `--slack-webhook-file`, and follows no
redirects. Of particular interest besides the above:

- a way for a page to run script, or for a rule, label or incident to
  change what a page shows beyond its own text;
- a way to make it change the ledger without a request to it, through a
  page from another site, or with `--read-only`;
- a way to read or write files outside its ledger's directory, or to make
  it show a token or a webhook's URL;
- a request, or a ledger, that makes it use memory or time far out of
  proportion;
- a way to change someone else's comment, to make a runbook or tracker
  link run script, or to make a form go back to another site;
- a way to change what a role doesn't allow, or to get a role without the
  proxy's headers (including as whoever it didn't name);
- a way to make a notice show a webhook's URL, notify everyone in Slack
  (`<!channel>`), or go somewhere other than the URL in its file.

`lapilli serve` reads by default, and changes only what a person approves.
With `--changes`, a change to an alert rule that an approver (`--approvers`,
or the admins) approved on its page is checked again, then handed to a
recipe that opens a pull request, with the token in `--changes-token-file`
on `--incidents-listen`, or, for a rule read from Grafana, applied through
Grafana's provisioning API with the token in `--changes-grafana-token-file`,
or, for a rule read from a ruler in a namespace `--changes-in` names,
through the ruler's API with its write credential
(`--changes-ruler-token-file`, `--changes-aws-role`,
`--changes-azure-client-id`), or, for a cloud's own alarm read with
`--alarms` that `--changes-in` names as `SOURCE:NAME`, through the cloud's
API with its write identity (`--changes-aws-role`,
`--changes-azure-client-id`, `--changes-google-service-account`,
`--changes-oci-config`, an OCI API key in a file). Lapilli holds no
repository credentials. Of particular interest:

- a way to approve a change other than as an approver, on the proposal's
  page, in a browser: through the API, MCP, a token, a page from another
  site, as whoever made the proposal, or twice as one person where two are
  asked for;
- a way to apply or hand out a change that wasn't approved, while changes
  are off or paused, past `--changes-limits`, or after the rule changed or
  its backtest got worse;
- a way to make it change a rule other than the one a proposal names, write
  anything but alert rules, or write to anywhere but the Grafana or the
  ruler it reads from; to write in a ruler's namespace or rule group
  `--changes-in` doesn't name, or one tagged as another tool's; to change,
  in a unit it writes whole (a group, an Amazon namespace, an Azure rule
  group), any other rule than as the ruler held it just before; or to make
  a recipe push, or write a file, other than the one the proposal was made
  against;
- a way to make it write, turn off or delete a cloud's alarm `--changes-in`
  doesn't name, one tagged as another tool's, one in another account,
  region, subscription, project or compartment than `--alarms` names, or
  one whose actions do more than tell (CloudWatch's Auto Scaling or EC2
  actions); to delete an alarm it doesn't replay; to rewrite an alarm
  losing a field it didn't know; to turn a rule off or delete it while it
  fires; or to silence an alarm (a mute rule, a suppression, a snooze, its
  notifications taken away) rather than change it;
- a way for the text of a fix to what a rule says to be written as anything
  but the value of the one annotation it names, or to change Grafana's own
  annotations;
- a way for the changes token to do anything but read the changes handed
  out and say what became of them;
- a way for an agent's words to pass as a person's, or for an agent to do
  anything but propose.

## Versions

Only the latest release gets fixes, in a patch release. Each release is
checked with [govulncheck](https://go.dev/doc/security/vuln/) before it is
published, the code and each binary, and the latest release is checked again
every day: a binary holds the Go standard library and modules it was built
with, so a vulnerability found in them later is fixed by the next release.
To check the binary you have (v0.5.1 and later):

```sh
govulncheck -mode=binary "$(command -v lapilli)"
```

There is one maintainer today ([MAINTAINERS.md](MAINTAINERS.md)), so fixes
are best effort.
