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
- Webhook authentication: `Authorization: Bearer <token>`, on by default. The chart generates
  the token (kept across upgrades) or uses `webhook.auth.existingSecret`; optional
  `webhook.networkPolicy`. `kairn demo` fires its alert from inside the controller pod.
  Authentication runs before the body is read; bodies are capped at 256 KiB and requests
  at 16 concurrent; rejections are counted and logged at most every 10 s. `/healthz` moved
  to its own port (8081) so a NetworkPolicy on the webhook port never blocks probes.

- `kairn verify s3://… | gs://… | https://…`: verifies an object straight from a bucket or
  a presigned URL, streamed without being stored, with the same verdicts and exit codes as
  a local file. S3 objects also get a version history check (a key written more than once,
  or with a delete marker, is FAILED; `--current-only` skips it), and the key's
  `<cluster>/<incident>.ieb` is checked against the bundle (`--any-key` skips it). New
  `--expect-sha256` (also for local files) and `--version-id`. Read failures are exit 3.
  See `docs/design-remote-verify.md`. `--no-default-features` builds the CLI without any
  network code; the controller image ships that build.
- `status.exports.<name>.sha256` and `.versionId`: the uploaded object's hash and store
  version, for `kairn verify --expect-sha256 / --version-id`.
- Export refuses to run under a cluster id that isn't `[A-Za-z0-9._-]` (at most 100): the
  id is a key segment. The chart fails the render in that case.

- `kairn verify --output json`: one `kairn.dev/verify-result/v1` document on stdout for
  every outcome (exit 0–3), with stable problem codes (`integrity`, `context`, `signature`,
  `custody`, …), the input's size and SHA-256, the bundle's own cluster and incident, and
  the bucket version history. Stable from v0.1.0: `spec/VERIFY-RESULT.md`. The fixtures now
  pin each case's problem codes too.
- `kairn-bundle`: `VerifyReport.problems` is now `Vec<Problem>` (`code` + `message`); new
  `cluster_id` and `incident_id` fields; new `verify_reader`.
- Verdict precedence is now explicit: FAILED > CANNOT_EVALUATE > PARTIAL > OK. So a
  `--expect-sha256` mismatch is FAILED (exit 1) even on a bundle that can't be evaluated
  (previously 3), and a bucket's unreadable version history no longer hides a FAILED bundle.
- An unusable `--key` (not a public key) is now "cannot evaluate" (exit 3, `unreadable`)
  instead of a signature failure (exit 1): it is the operator's input, not evidence of
  tampering.
- `--expect-sha256` on a directory is a usage error (exit 64; previously 3).
- A local `.ieb` is read once: the SHA-256 reported and checked is of exactly the bytes
  verified.
- `kairn verify s3://…` on a deleted key (delete markers, no current object) is FAILED
  (`custody`) instead of "no such object".
- The manifest's `schema_version` is dispatched before the duplicate-member check
  (IEB-SPEC §9).

### Migration
- Alertmanager must now send the webhook token: add `http_config.authorization.
  credentials_file` to the Kairn receiver (the chart NOTES show how to copy the token), or
  set `webhook.auth.enabled=false` (not recommended).
- `kairn demo --webhook-service` was removed (the demo no longer uses the Service).

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
