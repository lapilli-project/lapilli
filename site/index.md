---
layout: default
title: A flight recorder for Kubernetes incidents
---

# Lapilli

The moment an alert fires, Lapilli captures the incident window — the events, the owner-chain YAML,
the logs from the container that *just died*, the metric shape, and what recently changed — into one
portable file you can verify offline, months later, on a laptop that has never seen the cluster.

**What it is, stated so it can be checked.** Lapilli is triggered by operational signals, correlates
Kubernetes-native state across the incident window, and seals it as an open, portable,
offline-verifiable evidence file — then reads that file itself (`lapilli verify`,
`lapilli postmortem`, `lapilli mcp`), depending on no observability vendor and no AI tool.

<div class="note" markdown="1">
**Where this actually is.** `v0.1.0`, released 2026-09-28, and `pre-alpha`: **one maintainer, no
adopters listed.** It works end to end on Kubernetes 1.30 and 1.37 in CI, and nobody but its author
has run it on a cluster they operate. If you are looking for a tool with a community behind it, this
is not that yet — and the roadmap says so in the same words.
</div>

## Install

```sh
helm install lapilli oci://ghcr.io/lapilli-project/charts/lapilli \
  -n lapilli-system --create-namespace --set clusterId=<name>
```

The CLI, either from a published tarball or from crates.io:

```sh
gh release download v0.1.0 -R lapilli-project/lapilli
cargo install lapilli
```

Check what you downloaded before you run it — a project whose claim is offline-verifiable evidence
should not ask you to take its own release on trust. Each tarball and `SHA256SUMS` carries a
Sigstore-signed build-provenance attestation:

```sh
gh attestation verify SHA256SUMS -R lapilli-project/lapilli
gh attestation verify lapilli-v0.1.0-*.tar.gz -R lapilli-project/lapilli
sha256sum -c SHA256SUMS
```

## The bundle format

A bundle is a zstd-compressed tar with a manifest, a content hash tree, and an optional detached
signature. Its identifier is `lapilli.dev/ieb/v1`, frozen from the first release, and the full
normative layout is at **[/ieb/v1](/ieb/v1)** — which is where that identifier points, for a human
reading a manifest. A verifier must never fetch it: verification is offline, and a verifier that
asked a website what the rules are would have given that property away.

The format is a *reference bundle layout*, not a standard. That word is earned when an independent
producer or consumer adopts it, and none has.

## What it does not do

It records a pod's incident window. Alerts that carry no `pod` label — replicas mismatch, rollout
stuck, node not ready, SLO burn rate — are counted and dropped, which is most of what an SRE is
actually paged for. Closing that gap is the first item of the next release, and the first proposal
for it was [refuted by review](https://github.com/lapilli-project/lapilli/blob/main/docs/design-review-round31.md)
rather than shipped.

Redaction is best-effort and never touches container logs, because logs are the evidence.
[What a bundle holds](https://github.com/lapilli-project/lapilli/blob/main/docs/data-handling.md) is
its own page, and reading it before installing is the point of writing it.

## Read further

- [Design and architecture](https://github.com/lapilli-project/lapilli/blob/main/DESIGN.md) — and the
  thirty-one adversarial review rounds under `docs/`, including the ones that killed features
- [What is left, in order](https://github.com/lapilli-project/lapilli/blob/main/ROADMAP.md)
- [Compatibility](https://github.com/lapilli-project/lapilli/blob/main/docs/COMPATIBILITY.md) — what
  is frozen and what is not
- [Security policy](https://github.com/lapilli-project/lapilli/blob/main/SECURITY.md)
