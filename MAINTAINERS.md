# Maintainers

| Name | GitHub ID | Company/Organization | Areas |
|---|---|---|---|
| jmcunst | [@JMcunst](https://github.com/JMcunst) | Independent | all |

Lapilli has one maintainer today. It needs more, and from more than one
organisation. A tool that judges everyone's alerts shouldn't depend on one
person's time, or on one company's view of what an alert is.

## Why you might

- **A readable codebase, with its reasons written down.** It is Go with no
  cgo: some 160,000 lines, and more again of tests. Most packages' tests
  take seconds; `go test ./...` takes ten to fifteen minutes, most of it
  serve's. The design notes in `docs/design/` say why things are as they
  are.
- **Real questions that are still open.** How faithfully can a replay
  reproduce what fired? How should a thin incident log still give a fair
  verdict? See [ROADMAP.md](ROADMAP.md).
- **Things we can show.**
  - Every release is checked for vulnerabilities before it is published
    and carries build provenance, and the latest is checked again every
    day.
  - Performance has budgets in the tests: over twelve weeks of 200,000
    firings, an hourly run within two seconds, and each page held to a few
    statements a rule, never one a firing.
- **Vendor neutral by rule.** It reads Prometheus, Thanos, Mimir, Cortex,
  VictoriaMetrics and Loki, the managed Prometheus of AWS, Azure and Google
  Cloud, Grafana, plain rule files and each cloud's own alarms (CloudWatch,
  Azure Monitor, Google Cloud Monitoring, OCI Monitoring), and judges
  Datadog's and New Relic's alerts by their history. Some are checked only
  against what their documentation shows, AWS's and Azure's among them
  ([ROADMAP.md](ROADMAP.md)): help with a cloud is help for everyone who
  uses it.

## How to become one

1. **Contribute in an area.** A fix, a rule that replays wrongly with what
   really fired, a translation or a recipe all count. Reports of a replay
   that differs from reality are the most useful thing there is
   ([CONTRIBUTING.md](CONTRIBUTING.md)).
2. **Review.** After a few merged changes in an area, a maintainer can ask
   you to review changes there. GitHub asks a code owner only if they have
   write access, so you are added to [CODEOWNERS](.github/CODEOWNERS) for
   the area's paths when you become a maintainer.
3. **Maintain.** After about three months of steady contributions and
   reviews, a maintainer nominates you in a pull request to this file. You
   become a maintainer when the maintainers agree, as
   [GOVERNANCE.md](GOVERNANCE.md) describes. You get write access and a
   share of triage; releases, security reports and reports under the Code
   of Conduct need the repository's admin role (the `release tags`
   ruleset, and GitHub's own).

A maintainer who steps back moves to the list below, and can come back the
same way.

## Emeritus

None yet.
