# Security Policy

Lapilli handles incident evidence and produces signed artifacts, so we take security
seriously — both in the project's own code and in the integrity guarantees it makes.

## Reporting a vulnerability

**Do not open a public issue for security vulnerabilities.**

Instead, report privately via [GitHub Security Advisories](https://github.com/lapilli-project/lapilli/security/advisories/new)
(preferred), or contact a maintainer through the address in [`MAINTAINERS.md`](MAINTAINERS.md).

Scanner findings against the published image are a different question from a vulnerability
report: what a Trivy or grype scan reports, what the binaries actually link, and why both base
images are pinned by digest are in [`docs/security-scanning.md`](docs/security-scanning.md).
Read that first — and do file a report if it contradicts that page.

Please include:

- a description of the vulnerability and its impact,
- steps to reproduce or a proof of concept,
- affected versions, if known.

We will acknowledge your report, work with you on a fix and a coordinated disclosure
timeline, and credit you (unless you prefer to remain anonymous).

## Scope of particular interest

Because Lapilli's value rests on evidence integrity, we are especially interested in reports
concerning:

- ways to forge, alter, or replay an Incident Evidence Bundle without detection,
- weaknesses in the signing / hash-tree / redaction chain,
- privilege escalation from the in-cluster controller,
- leakage of secret values into a bundle despite redaction policy.

## Supported versions

This section is the single source for supported release lines (see
[`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md)).

| Release line | Status |
|---|---|
| **v0.2.x** (current) | eligible for fixes, including security fixes, best effort |
| v0.1.x | **not supported.** Before 1.0 only the latest minor is, and there is no backport |

**If you are on v0.1.0**, upgrade: `0.2.0` carries a fix for a defect in this document's own scope —
*leakage of secret values into a bundle despite redaction policy*. With `redaction.mode: strict` and
a non-empty `redaction.plaintext`, an exempted name skipped redaction entirely rather than skipping
only strict's widening, so a name that looks like a credential came through in the clear where
`mode: default` removes it. The CHANGELOG's **Migration** section for `0.2.0` says how to tell from
a bundle whether it is affected, without opening `resources/`.

Lapilli has one maintainer today, so fixes are best effort and without a guaranteed response
time. A security fix to `lapilli verify` is intended to ship in a release that still reads
every released bundle format, so upgrading the verifier is the remedy for any affected
version; if a fix ever has to stop reading something, its advisory says so. Advisories are
published as GitHub Security Advisories. Private vulnerability reporting must be enabled in
the repository settings before the first release (see `RELEASE.md`).
