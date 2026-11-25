# Governance

## Principles

These hold for every change, and a change that breaks one needs a reason
written down in its pull request:

- **Lapilli reads by default, and changes only what a person approves.**
  Out of the box it queries APIs and reads files, and changes nothing it is
  pointed at. With changes turned on, it proposes changes to alert rules,
  and applies one only once a person with the role for it has approved it:
  as a pull request to where the rule is kept, or, for a rule kept nowhere
  else, through the API that holds it, with the rule as it was kept so the
  change can be undone. It never silences an alert, writes to Prometheus or
  a cluster beyond the rules it was told it may change, or applies what an
  agent proposes without a person's approval
  ([docs/design/changes.md](docs/design/changes.md)).
- **Vendor neutral.**
  - Every Prometheus-compatible API, and the managed Prometheus of each
    cloud, is supported alike. A feature isn't built for one cloud when the
    others could have it.
  - Integrations are generic: a webhook, a token and an API, with Slack the
    one tool lapilli posts to by name. A particular tool's is a recipe
    beside it (`recipes/`), tested against a server standing in for it.
- **Measured.** Claims about speed are tests with budgets, and claims about
  faithfulness are replays checked against what really fired.
- **Safe by default.**
  - The UI runs no scripts.
  - Secrets are read from files and never shown.
  - Nothing is sent anywhere it wasn't told to go.

## Decisions

- **Day-to-day changes** are made in pull requests, and merge once the
  required checks pass. With two maintainers or more, a change needs one
  maintainer's approval, and a maintainer's own change waits a working day
  for objections, unless it fixes something broken. With one, as now, the
  maintainer merges their own.
- **Larger changes are written down in `docs/design/` first, or in the same
  pull request.** These are a new command, a new port or token, a new kind
  of data in the ledger, or a change to how a firing is judged. With two
  maintainers or more, the design is open for at least a week; with one, it
  may be merged sooner, and its note says so.
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

- **Who:** a maintainer with the admin role, which tags `v*` need (the
  `release tags` ruleset).
- **How:** after a dry run of the release workflow on `main`, push a tag
  `vX.Y.Z` on a commit of `main` whose CI run passed in full. The workflow
  tests and builds the binaries, the image and the chart, checks the code
  and each binary with govulncheck before publishing, and attests their
  provenance; the latest release is checked again every day.
- **When:** what adds or changes something waits for the next minor
  release, and there are at most two of those a week, each with everything
  merged since the last. A fix for a vulnerability, or for what a release
  broke, doesn't wait: it is a patch release as soon as it is ready. A
  release that changes the ledger's schema says so at the top of its
  changelog entry, since a release from before can't open the ledger after
  ([docs/upgrading.md](docs/upgrading.md)).
- **Numbering:** while the version is 0.x, a minor release may change flags
  or the API, and its changelog entry says how. A security fix may change
  behaviour in a patch release too; its entry says how. What keeps its
  name and meaning across releases (the report's fields, the metrics, the
  CSV's columns), and how one of them is changed when it must be, is listed
  in [docs/numbers.md](docs/numbers.md#what-stays-the-same). What 1.0
  keeps, and what comes before it, is in
  [ROADMAP.md](ROADMAP.md#toward-10).

## Conduct

Everyone taking part in the project, in its repositories, discussions and
anywhere it is represented, follows the
[Code of Conduct](CODE_OF_CONDUCT.md). The maintainers enforce it, and
keep who reported what to themselves.

## Security

Vulnerabilities are reported privately, as [SECURITY.md](SECURITY.md)
describes. The maintainers handle them in private and release a fix before
the advisory is published.

## Changing this document

By a pull request that a majority of the maintainers approve.
