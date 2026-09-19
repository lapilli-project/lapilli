# Changelog

All notable changes to Kairn are listed here. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Kairn follows SemVer; before 1.0 a minor release may change alpha surfaces (CRDs, chart
values, CLI commands other than `kairn verify`). Compatibility commitments are in
[`docs/COMPATIBILITY.md`](docs/COMPATIBILITY.md). Changes that need action on upgrade are
listed under **Migration**.

## [Unreleased]

### Added
- Object-store export (S3, S3-compatible, GCS): after sealing, each bundle is copied to
  admin-defined destinations (`export.destinations` in the chart; profiles reference them by
  name). Conditional create with a service-verified SHA-256, never overwriting; an existing
  object with other bytes is a `conflict`. Retries with backoff; per-destination status
  (`status.exports`, `EXPORT` column) and Events. Only a verified bundle of the capture is
  ever uploaded. `kairn demo` captures stay local. See `docs/design-export.md`.
- Chart: `serviceAccount.annotations` (IRSA, GKE Workload Identity).

## [0.1.0] - unreleased

First release. The bundle format is `kairn.dev/ieb/v1` and is frozen from this release.

### Added
- Alertmanager webhook → `IncidentCapture` → collectors (logs incl. the previous container
  instance, resources, events + timeline, change indicators, optional PromQL metrics) →
  sealed, portable `.ieb` bundle.
- `diffs/`: before/after pod-template diffs of every rollout in the capture window, for
  Deployments, StatefulSets and DaemonSets, from the revision history Kubernetes already
  keeps; opt-in key-level diff of ConfigMaps whose referenced name changed.
- Redaction v1 at capture time, recorded in `redaction.json` (`default` / `strict` / `off`).
- `kairn verify` (exit codes 0 OK / 1 FAILED / 2 PARTIAL / 3 cannot evaluate / 64 usage),
  optional static-key ECDSA signing, `kairn keygen`, `kairn demo`, `kairn unpack`.
- Helm chart with a values schema; tested on Kubernetes 1.30 and 1.37.

### Security
- `kairn verify` streams `.ieb` files instead of extracting them, enforces path rules,
  rejects links, duplicate and case-colliding entries, and unlisted files under
  `signature/`, and applies resource limits.
- The signing declaration is part of the signed manifest; authenticity is established only
  with `--key`.

### Migration
- Development builds before v0.1.0 wrote `kairn.dev/ieb/v0` bundles, which no release
  reads (`kairn verify` exits 3).
