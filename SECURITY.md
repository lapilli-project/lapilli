# Security

Report a vulnerability privately through
[GitHub Security Advisories](https://github.com/lapilli-project/lapilli/security/advisories/new),
not in a public issue.

`lapilli verdict` reads rule, incident and firing files, and queries the
Prometheus-compatible API it is given, with the token it is given or, with
`--auth`, the AWS, Azure or Google Cloud credentials the machine already
has. With `--grafana` it reads the alert rules a Grafana holds, with the
token in `--grafana-token-file`. It sends its report only where it is told
to: a Pushgateway (`--push`) and a Slack webhook (`--slack-webhook-file`). Of
particular interest:

- a way to make it send a query, a token, a credential or a signature
  anywhere other than the API or the Grafana it was given, or the report
  anywhere other than where it was told;
- a way to make it show a Slack webhook's URL or a token, or a report make
  Slack notify people (a mention, `<!channel>`);
- a way for a rule file, an incident file, a firing file or an API answer to
  make it write a file, run a command, or send escape sequences to your
  terminal;
- a rule or answer that makes it use memory or time far out of proportion.

`lapilli serve` does the same every hour, and serves a web UI and API that
read and write its ledger (one SQLite file). It has no sign-in of its own:
it listens on localhost unless told otherwise, trusts the header named by
`--user-header` for who made a change, refuses posts from other sites, and
with `--read-only` takes no changes. Of particular interest besides the
above:

- a way for a page to run script, or for a rule, label or incident to
  change what a page shows beyond its own text;
- a way to make it change the ledger without a request to it, through a
  page from another site, or with `--read-only`;
- a way to read or write files outside its ledger's directory, or to make
  it show a token or a webhook's URL;
- a request, or a ledger, that makes it use memory or time far out of
  proportion.

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

There is one maintainer, so fixes are best effort.
