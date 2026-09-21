# Lapilli Governance

Lapilli aims to be a community-driven, vendor-neutral project. This document describes how
the project is governed today and the path to broader open governance as the community
grows. It is intended to evolve with the project.

## Current state

Lapilli is in its founding phase with a single maintainer (see [`MAINTAINERS.md`](MAINTAINERS.md)).
This document sets the rules the project commits to now so that governance does not have
to be invented reactively later.

## Principles

- **Vendor neutrality.** No single company controls Lapilli. Decisions are made in the open,
  on the project's public issue tracker and discussions.
- **Open by default.** Design discussions, roadmaps, and decisions happen in public.
- **Meritocracy.** Influence is earned through sustained, high-quality contribution —
  code, review, documentation, triage, or community support.

## Roles

### Contributors

Anyone who contributes to the project — code, docs, issue triage, reviews, or design
discussion. No formal process; opening a pull request or issue makes you a contributor.

### Maintainers

Maintainers have write access and are responsible for the technical direction, review and
merge of contributions, releases, and community health. The current maintainer list lives
in [`MAINTAINERS.md`](MAINTAINERS.md).

**Becoming a maintainer.** A contributor with a sustained track record (typically several
months of substantial, merged contributions and helpful review) may be nominated by an
existing maintainer. With more than one maintainer, nomination requires a supermajority
(2/3) approval of current maintainers. This project explicitly intends to add maintainers
from outside the founder's employer to establish genuine multi-organization governance.

**Stepping down / inactivity.** Maintainers may step down at any time. A maintainer
inactive for an extended period (typically 6 months) may be moved to emeritus status.

## Decision making

- **Lazy consensus** is the default. Most decisions are made through normal PR review;
  changes merge when a maintainer approves and no other maintainer objects.
- **Significant changes** (architecture, the IEB format spec, the compatibility policy in
  `docs/COMPATIBILITY.md`, breaking API/CRD changes, governance) are proposed as an issue or design document and require explicit maintainer
  sign-off, with reasonable time for community input. For the compatibility policy that
  window is at least one minor release of notice in the CHANGELOG (security changes are
  exempt and ship with an advisory).
- When consensus cannot be reached, a maintainer vote decides; with multiple maintainers,
  a simple majority carries, with the caveat that maintainers should strongly prefer
  consensus over votes.

## Code of Conduct

All participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Changing this document

Changes to governance follow the "significant changes" process above and must be approved
by the maintainers.
