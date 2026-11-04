# Maintainers

| Maintainer | Areas |
|---|---|
| [@JMcunst](https://github.com/JMcunst) | all |

Lapilli has one maintainer today. It needs more, and from more than one
organisation. A tool that judges everyone's alerts shouldn't depend on one
person's time, or on one company's view of what an alert is.

## Why you might

- **A small, readable codebase.** It is Go with no cgo; `go test ./...` runs
  the whole of it in seconds. The design notes in `docs/design/` say why
  things are as they are.
- **Real questions that are still open.** How faithfully can a replay
  reproduce what fired? How should a thin incident log still give a fair
  verdict? See [ROADMAP.md](ROADMAP.md).
- **Things we can show.**
  - Every release is checked for vulnerabilities before it is published,
    carries build provenance, and is checked again every day.
  - Performance has budgets in the tests: an hourly run over two million
    firings takes under a fifth of a second.
- **Vendor neutral by rule.** Prometheus, Thanos, Mimir and VictoriaMetrics
  work, as do the managed Prometheus of AWS, Azure and Google Cloud, Grafana
  and plain rule files. Help with a cloud is help for everyone who uses it.

## How to become one

1. **Contribute in an area.** A fix, a rule that replays wrongly with what
   really fired, a translation or a recipe all count. Reports of a replay
   that differs from reality are the most useful thing there is
   ([CONTRIBUTING.md](CONTRIBUTING.md)).
2. **Review.** After a few merged changes in an area, a maintainer can add
   you to [CODEOWNERS](.github/CODEOWNERS) for its paths, so you are asked
   to review changes there.
3. **Maintain.** After about three months of steady contributions and
   reviews, a maintainer nominates you in a pull request to this file. You
   become a maintainer when the maintainers agree, as
   [GOVERNANCE.md](GOVERNANCE.md) describes. You get write access and a
   share of releases, triage and security reports.

A maintainer who steps back moves to the list below, and can come back the
same way.

## Emeritus

None yet.
