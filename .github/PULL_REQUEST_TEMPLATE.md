<!--
Thank you. Two things make a review fast here, and both are about evidence rather than effort.
Delete any section that does not apply.
-->

## What this changes, and why

<!-- The motivation, not the diff. If it fixes a defect, say how the defect showed itself. -->

## How it was verified

<!--
Name what you ran, not that you tested it. For example:

  scripts/release-check.sh           # the local gate
  scripts/release-check.sh --e2e     # adds kind, needs docker
  cargo test -p <crate>              # one crate

If the change adds or tightens a check, say how you know the check catches what it claims:
break the thing it governs, watch it fail, put it back. This project calls that a mutation proof
and asks for it at the site the check governs, not next to it.

If only a cluster can judge the change — anything in the controller's reconcile path, the notify
hand-off, retention, export, or the E2E's own assertions — say so. `ci` runs kind on every pull
request, and a green local gate has been wrong about exactly those paths four times.
-->

## Compatibility

<!--
Does this touch a surface `docs/COMPATIBILITY.md` commits to? The `ieb/v1` format, `lapilli verify`'s
output and exit codes, the CRDs, chart values, or the metric series names. If yes, say which and
what an existing user would have to do.
-->

## Checklist

- [ ] Every commit is signed off (`git commit -s`) — the DCO check is a required one
- [ ] `CHANGELOG.md` has an entry under `## [Unreleased]`, with a **Migration** note if an upgrade needs action
- [ ] The documents that state what this code does are updated in the same pull request
