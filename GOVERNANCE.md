# Governance

## Principles

These hold for every change, and a change that breaks one needs a reason
written down in its pull request:

- **Lapilli only reads.** It queries APIs and reads files. It never changes
  a rule, an alert, a cluster or anything else it is pointed at. What it
  suggests goes to reports, and to tasks for people.
- **Vendor neutral.**
  - Every Prometheus-compatible API, and the managed Prometheus of each
    cloud, is supported alike. A feature isn't built for one cloud when the
    others could have it.
  - Integrations are generic: a webhook, not a particular tracker.
- **Measured.** Claims about speed are tests with budgets, and claims about
  faithfulness are replays checked against what really fired.
- **Safe by default.**
  - The UI runs no scripts.
  - Secrets are read from files and never shown.
  - Nothing is sent anywhere it wasn't told to go.

## Decisions

- **Day-to-day changes** are made in pull requests. One maintainer's approval
  merges a change; a maintainer's own change waits a working day for
  objections, unless it fixes something broken.
- **Larger changes go to `docs/design/` first.** These are a new command, a
  new kind of data in the ledger, or a change to how a firing is judged. The
  design is a pull request, open for at least a week.
- **Disagreements** that discussion doesn't settle are decided by a majority
  of maintainers. With an even split, the change doesn't go in.

## Maintainers

- **Who they are:** the people in [MAINTAINERS.md](MAINTAINERS.md).
- **Joining:** maintainers are added as that file describes. Adding one
  needs the agreement of a majority of the current maintainers, and no
  objection from any of them for a week.
- **Leaving:** a maintainer who has been inactive for six months, or who
  asks, moves to emeritus.
- **Organisations:** while there are three maintainers or more, no
  organisation should hold a majority of them.

## Releases

- **Who:** any maintainer can release.
- **How:** push a tag `vX.Y.Z` on `main` after CI passes. The release
  workflow builds the binaries, the image and the chart. It checks them with
  govulncheck before publishing, and attests their provenance.
- **Numbering:** while the version is 0.x, a minor release may change flags
  or the API, and its changelog entry says how.

## Security

Vulnerabilities are reported privately, as [SECURITY.md](SECURITY.md)
describes. The maintainers handle them in private and release a fix before
the advisory is published.

## Changing this document

By a pull request that a majority of the maintainers approve.
