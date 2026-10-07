---
layout: default
title: Kubernetes incidents as files you can check later
---

# Lapilli

**Lapilli turns a Kubernetes incident into a file that can be checked later without the cluster it
happened on** — sealed as evidence for the people who review it, and frozen as a case for the
agents asked to explain it. Two tools:

- **The recorder** (`v0.2.0`, released) is what this page installs: a flight recorder for
  Kubernetes incidents.
- **Cases** (`lapilli case`) are pre-alpha and in no release: an incident frozen together with its
  answer key, replayed with no cluster, so that an agent that investigates incidents can be graded
  on how it investigated. They live in the
  [repository](https://github.com/lapilli-project/lapilli/blob/main/docs/design-case.md), not here
  yet.

## The recorder

The moment an alert fires, it **seals** the incident window into one portable file you can
verify offline, months later, on a laptop that has never seen the cluster — the events, the
owner-chain YAML, the logs from the container that *just died*, what recently changed, and
optionally the metric shape.

The seal is the part nobody else ships. The collection is not: Robusta is MIT-licensed, fires on the
same Alertmanager webhook, and has an enricher for each of those. If you want the enrichment routed
to Slack, use Robusta. Lapilli is for when you need the window as a **file you own** that verifies
without credentials to anything.

**What it is, stated so it can be checked.** The recorder is triggered by operational signals,
gathers Kubernetes-native state across the incident window, and seals it as an open, portable,
offline-verifiable evidence file — then reads that file itself (`lapilli verify`,
`lapilli postmortem`, `lapilli mcp`), depending on no observability vendor and no AI tool.

<div class="note" markdown="1">
**Where this actually is.** `v0.2.0`, released 2026-10-01, and `pre-alpha`: **one maintainer, no
adopters listed.** It works end to end on Kubernetes 1.30 and 1.37 in CI, and nobody but its author
has run it on a cluster they operate. If you are looking for a tool with a community behind it, this
is not that yet — and the roadmap says so in the same words.
</div>

## Install

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli \
  -n lapilli-system --create-namespace --set clusterId=<name>
```

If you installed `v0.1.0`, upgrade. With `redaction.mode: strict` **and a non-empty
`redaction.plaintext`**, it redacted *less* than the default mode would have, leaving credentials in
bundles that were meant to be the most tightly redacted —
[GHSA-7994-x9mx-vx43](https://github.com/lapilli-project/lapilli/security/advisories/GHSA-7994-x9mx-vx43)
(low). Upgrading does not change bundles `v0.1.0` already sealed; those have to be re-examined or
destroyed.

The CLI, either from a published tarball or from crates.io:

```sh
gh release download v0.2.0 -R lapilli-project/lapilli
cargo install lapilli
```

Check what you downloaded before you run it — a project whose claim is offline-verifiable evidence
should not ask you to take its own release on trust. Each tarball and `SHA256SUMS` carries a
Sigstore-signed build-provenance attestation:

```sh
gh attestation verify SHA256SUMS -R lapilli-project/lapilli
gh attestation verify lapilli-v0.2.0-*.tar.gz -R lapilli-project/lapilli
sha256sum -c SHA256SUMS
```

## The bundle format

A bundle is a zstd-compressed tar with a manifest, a content hash tree, and an optional detached
signature. Its identifier is `lapilli.dev/ieb/v1`, frozen from the first release, and the full
normative layout is at **[/ieb/v1]({{ '/ieb/v1' | relative_url }})** — which is where that identifier points, for a human
reading a manifest. A verifier must never fetch it: verification is offline, and a verifier that
asked a website what the rules are would have given that property away.

The format is a *reference bundle layout*, not a standard. That word is earned when an independent
producer or consumer adopts it, and none has.

## What it does not do

It records a pod's incident window. Alerts that carry no `pod` label — replicas mismatch, rollout
stuck, node not ready, SLO burn rate — are counted and dropped, which is most of what an SRE is
actually paged for. So are alerts that carry a `pod` label which is not their subject's: against
kube-prometheus-stack, **43 of 155 rules** arrive with the pod that prometheus-operator relabelled
on from the scrape target, so the pod is kube-state-metrics' or node-exporter's. Those used to be
accepted and sealed under the wrong pod's name; they are now refused and counted, which made the
coverage number worse and the bundles true.

Closing the `pod`-label gap is the first item of the next release, and the first proposal for it was
[refuted by review](https://github.com/lapilli-project/lapilli/blob/main/docs/design-review-round31.md)
rather than shipped.

Redaction is best-effort and never touches container logs, because logs are the evidence.
[What a bundle holds](https://github.com/lapilli-project/lapilli/blob/main/docs/data-handling.md) is
its own page, and reading it before installing is the point of writing it.

## Read further

- [Design and architecture](https://github.com/lapilli-project/lapilli/blob/main/DESIGN.md) — and the
  adversarial review rounds under `docs/`. They are a record of what was examined, not evidence of
  quality: every lens in them was Claude-family, so they are calibration and not independent
  review. The ones that killed features, and
  [round 35](https://github.com/lapilli-project/lapilli/blob/main/docs/design-review-round35.md),
  which found three of this project's headline claims false, are the useful ones to read
- [What is left, in order](https://github.com/lapilli-project/lapilli/blob/main/ROADMAP.md)
- [Compatibility](https://github.com/lapilli-project/lapilli/blob/main/docs/COMPATIBILITY.md) — what
  is frozen and what is not
- [Security policy](https://github.com/lapilli-project/lapilli/blob/main/SECURITY.md)
