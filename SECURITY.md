# Security Policy

Kairn handles incident evidence and produces signed artifacts, so we take security
seriously — both in the project's own code and in the integrity guarantees it makes.

## Reporting a vulnerability

**Do not open a public issue for security vulnerabilities.**

Instead, report privately via [GitHub Security Advisories](https://github.com/JMcunst/kairn/security/advisories/new)
(preferred), or contact the maintainers listed in [`MAINTAINERS.md`](MAINTAINERS.md).

Please include:

- a description of the vulnerability and its impact,
- steps to reproduce or a proof of concept,
- affected versions, if known.

We will acknowledge your report, work with you on a fix and a coordinated disclosure
timeline, and credit you (unless you prefer to remain anonymous).

## Scope of particular interest

Because Kairn's value rests on evidence integrity, we are especially interested in reports
concerning:

- ways to forge, alter, or replay an Incident Evidence Bundle without detection,
- weaknesses in the signing / hash-tree / redaction chain,
- privilege escalation from the in-cluster controller,
- leakage of secret values into a bundle despite redaction policy.

## Supported versions

Kairn is pre-alpha; there is no supported release line yet. This policy will be updated
when the first release is cut.
