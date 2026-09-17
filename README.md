# Kairn

> **A flight recorder for Kubernetes incidents.** The moment an alert fires, Kairn captures
> the full incident window — events, owner-chain YAML, the logs from the container that
> *just died*, the metric shape, and what recently changed — into **one portable file you
> own**. Stop reconstructing timelines from memory and screenshots.

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
![Status](https://img.shields.io/badge/status-pre--alpha-orange.svg)
![Language](https://img.shields.io/badge/built%20with-Rust-000000.svg)

When you get paged at 3 a.m., the logs, events, and "what changed" you need are already
rotating away. Kairn is always watching: the instant a Prometheus/Alertmanager alert fires,
it snapshots the incident window into a self-contained **Incident Evidence Bundle (IEB)** —
a portable file you own, that any tool can read and no vendor can hold hostage.

And because the record is captured automatically the moment it matters, it also serves as
**audit-supporting** evidence you used to assemble by hand — a bonus for regulated users,
not the pitch (see [why below](#the-honest-pitch)).

## The problem

- **The evidence horizon.** By the time a human logs in, Kubernetes events (≈1h TTL), the
  crashed container's logs, and the pre-incident metric shape are gone. Manual `must-gather`
  tools can't help — nobody's awake to run them in time.
- **Timeline archaeology.** Post-incident reviews are rebuilt from memory, Slack scrollback,
  and screenshots. There's no single artifact that *is* the incident.

## What Kairn is not

- ❌ another dashboard (Grafana, Komodor, Coroot)
- ❌ a metrics/logs store — it *reads* from Prometheus/K8s, never replaces them
- ❌ an AI auto-RCA narrative (HolmesGPT, k8sgpt) — Kairn produces the evidence they *consume*
- ❌ a manual diagnostic collector (troubleshoot.sh, must-gather)
- ❌ a security syscall/memory dump (Falco Talon + CRIU, Sysdig captures)

## The honest pitch

"We sign it" and "we capture automatically" are **not** novel — both ship today (Talon+CRIU,
Sysdig captures, Kosli, troubleshoot.sh + cosign). Kairn's edge isn't an architectural moat;
it's the one combination nobody offers as a single **open, operational** tool —

> triggered by **operational/reliability** signals · captured **at alert-time-plus-seconds,
> before the volatile evidence finishes rotating** · merged into **one portable file you
> own**, vendor-neutral, that any tool can read.

Vendor-neutral, portable incident evidence any tool can produce and consume is shared
infrastructure — that's the why-CNCF, and it holds without claiming "standard" today.

Full landscape and the two-round adversarial review that shaped this: [`DESIGN.md`](DESIGN.md),
[`docs/design-review-round1.md`](docs/design-review-round1.md),
[`docs/design-review-round2.md`](docs/design-review-round2.md).

## Integrity, stated honestly

**Signing is optional and off by default.** The load-bearing integrity feature is
`kairn verify` — it recomputes the bundle's hash tree, checks the bound incident context
(failing closed on mismatch), and flags partial captures, with or without a signature. When
you *do* enable signing (static-key ECDSA, cosign-compatible), you get **integrity after
sealing** + **producer authenticity**; in that config sealing time is self-asserted (an
independent time anchor and transparency log are later, opt-in additions). Kairn does **not**
claim the contents are a complete, faithful representation of cluster state — no signature
can. It is **one link** in a chain of custody the deploying org completes with WORM storage,
key custody, and access logging. See [`DESIGN.md` §5](DESIGN.md).

## Status & roadmap

Pre-alpha, under active design.

- **v0.1** — flight recorder on alert: Alertmanager trigger · `IncidentCapture`/`CaptureProfile`
  CRDs · K8s-API/events/log-tail collectors · change *indicators* · **optional** static-key
  signing (cosign v2 compatible) · PVC export · `kairn verify` + `kairn demo`.
- **v0.2+** — PromQL metric window, real spec change-diff, KMS signing, S3/OCI export,
  consumer adapters; keyless + Rekor + RFC 3161 TSA in v0.3. (eBPF causality is long-term
  research, out of scope for now.)

See [`DESIGN.md`](DESIGN.md) for the full plan.

## Contributing

Apache-2.0. All commits require a [DCO](https://developercertificate.org/) `Signed-off-by`
line (`git commit -s`). See [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`GOVERNANCE.md`](GOVERNANCE.md).

## License

[Apache License 2.0](LICENSE).
