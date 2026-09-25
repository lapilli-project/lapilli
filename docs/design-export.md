# Design — object-store export (v0.2)

Status: **implemented** (`crates/lapilli-controller/src/export.rs`; kind E2E `test/e2e/export.sh`
against MinIO; landed as 82f235f). v1 came out of one loop-engineering round
(log: [`design-review-round6.md`](design-review-round6.md)); the text below is that design, with
dated notes where the code settled something differently.

## Problem

Bundles land only on the controller's PVC: one volume, in the cluster whose incident is
being recorded, readable by anyone who can exec into the controller, gone with the cluster.
DESIGN §5 already says audit-grade custody needs storage the deploying organization
controls; Lapilli needs a way to put each bundle there.

## Goals / non-goals

- **Goal:** after sealing, copy each `.ieb` to an object store (S3, S3-compatible such as
  MinIO, GCS) once per incident, never overwriting an existing object.
- **Goal:** the export can't be turned into an exfiltration channel by whoever can edit a
  `CaptureProfile` or create an `IncidentCapture`.
- **Goal:** failures are visible, retried, and never lose the local bundle.
- **Non-goal:** managing buckets, lifecycle or retention (documented instead); Azure (later,
  same shape); more than one replica of the controller (not supported: no leader election).

## Design

### Destinations are defined by the admin, referenced by name

The chart (the admin) defines complete destinations; nothing outside the controller's
namespace can create or alter one (anyone who can edit that namespace's ConfigMaps and
restart the pod is, in effect, the admin):

```yaml
# values.yaml
export:
  destinations:
    - name: evidence
      url: s3://incident-evidence/prod-apne2   # bucket + prefix; gs://… for GCS
      region: ap-northeast-2
      endpoint: ""            # S3-compatible endpoint (MinIO); empty = the cloud's own
      allowHttp: false        # plaintext only for in-cluster test endpoints
      credentialsSecret: ""   # Secret with access_key_id / secret_access_key; empty =
                              # ambient (IRSA, EKS Pod Identity, GKE Workload Identity)
serviceAccount:
  annotations: {}             # e.g. eks.amazonaws.com/role-arn: …
```

- The chart renders them into a ConfigMap the controller reads at start (a change rolls the
  pod). A `CaptureProfile` lists destination **names**
  (`spec.export.destinations: [evidence]`); unknown names are `refused`. Profiles can't set
  URLs, endpoints, regions, TLS, or credentials.
- The default profile references every chart destination, so configuring one place is
  enough.
- `get` on each `credentialsSecret` is granted by `resourceNames`, from the same values.

### Object key, integrity, no-overwrite

- Key: `<prefix>/<cluster_id>/<incident_id>.ieb`, with `cluster_id` from the **controller's
  configuration**. A capture whose `spec.clusterId` differs is `refused`: creating an
  `IncidentCapture` must not let anyone write under another cluster's prefix.
- On S3 the upload carries a SHA-256 checksum the service verifies
  (`x-amz-checksum-sha256`, which Object Lock buckets also require); GCS relies on TLS and
  the read-back comparison below. Every upload is a **conditional create**
  (`If-None-Match: *` on S3, `ifGenerationMatch=0` on GCS).
- If the object already exists, Lapilli compares its size, then downloads and hashes it:
  equal → `uploaded`
  (an earlier attempt succeeded); different → `conflict`, surfaced and never "fixed".
- **Conditional-write probe.** Before a destination's first upload, Lapilli creates
  `<prefix>/<cluster_id>/.lapilli-probe` with a conditional create and then tries again; if the
  second attempt is not refused, the backend ignores conditional writes and the destination
  is disabled (logged, and every capture using it is `refused`). No delete is needed.

### Only verified bundles leave the cluster

Before each upload the file is verified as an `ieb/v1` bundle of this very capture
(matching cluster and incident, verdict OK or PARTIAL). The path comes from the capture's
status, which anyone allowed to patch that status could point at another file; without this
check the export would upload arbitrary controller files. A file that fails is `refused`
(`not-a-verified-bundle`).

### Order, retries, status

1. Seal and pack the local `.ieb`; patch `phase: Exported`. A capture is captured once:
   after `Exported`, spec edits never re-capture it (the local bytes must stay the ones
   uploaded).
2. Then, for each referenced destination, upload **from the local file**. Reconcile is only
   a no-op once every destination is settled (`uploaded`, `refused`, `conflict`, or out of
   attempts); otherwise it requeues at the next retry time.
3. Retries: exponential backoff 30 s … 1 h with ±20 % jitter, 24 attempts. Each call has a
   30 s timeout and at most 3 quick retries inside the client, so the reconcile loop owns
   long retries. At most **one upload at a time** per controller.
4. Status (alpha, additive) is a **map keyed by destination name**, so a merge patch touches
   only its own entry:
   `status.exports.<name> = {url, state, attempts, lastAttemptAt, reason, uploadedAt, sha256, versionId}`.
   `url` is the full object URL; `sha256` and `versionId` are the anchors remote verify pins
   (`crd.rs`, `ExportStatus`; [`design-remote-verify.md`](design-remote-verify.md)). `reason`
   is a fixed code; details go to the controller log only (no account IDs or ARNs in status).
   *Update (2026-09-25):* the codes as built (`export.rs`) are `not-allowed`,
   `cluster-mismatch`, `invalid-incident-id`, `invalid-cluster-id`, `destination-misconfigured`,
   `credentials-unavailable`, `conditional-writes-unsupported`, `local-bundle-unreadable`,
   `not-a-verified-bundle`, `too-large`, `access-denied` and `error`. v1's list had
   `unreachable` and `conflict` here too: a conflict is a `state`, not a reason, and a transport
   failure is `error` with the cause in the log.
5. A Kubernetes Event is emitted on `refused`, `conflict`, and when attempts run out; a
   printcolumn shows the first destination's state.
6. v0.2 reads the bundle into memory to upload it and refuses bundles over 100 MiB
   (`too-large`); multipart upload is later.

### Demo captures stay local

An alert label `lapilli.dev/export: local` (set by `lapilli demo`) makes the capture skip remote
destinations (`spec.skipRemoteExport: true`, shown as `local-only`), so demo runs never
land in a WORM bucket. The label is part of the capture's identity: a forged "local" alert
gets its own capture and can't claim the real one.

### Configuration is checked at start, connections at first use

A bad URL or scheme is permanent (`destination-misconfigured`). Credentials and clients are
set up at the first upload, so a Secret that isn't synced yet (ExternalSecrets, an API blip)
is retried (`credentials-unavailable`) rather than disabling the destination.

### What "never overwritten" means (honest)

Conditional create stops Lapilli itself and accidents from replacing a bundle. It does not
stop an attacker who holds credentials with more rights than Lapilli's: on a versioned bucket
a delete marker makes the key free again. Tamper evidence therefore needs, from the
organization, an Object Lock bucket in **compliance** mode (governance mode can be bypassed
by `s3:BypassGovernanceRetention`), a bucket policy that denies `PutObject` without the
`s3:if-none-match` condition, and a check of all object versions when reading evidence.

### Least-privilege credentials (documented policy)

Per cluster: allow `s3:PutObject` and `s3:GetObject` on `arn:aws:s3:::<bucket>/<prefix>/<cluster_id>/*`
only (no `ListBucket`: a missing object then reads as 403, which Lapilli handles); explicitly
deny `s3:DeleteObject*`, `s3:PutObjectRetention`, `s3:PutObjectLegalHold`,
`s3:BypassGovernanceRetention`, `s3:PutBucket*`; plus `kms:GenerateDataKey` for SSE-KMS
buckets. GCS: `roles/storage.objectCreator` + `storage.objects.get` on the bucket, with a
retention policy.

Readers (`lapilli verify s3://…`): `s3:GetObject` and `s3:GetObjectVersion` on the prefix, and
`s3:ListBucketVersions` on the bucket (the version history check; see
[`design-remote-verify.md`](design-remote-verify.md)). Keep them separate from the
controller's credentials.

## Testing

- Unit: destination resolution (unknown names, cluster mismatch), key construction, retry
  schedule, state transitions, reason mapping.
- kind E2E with MinIO (a bucket created with object lock): the bundle lands under the
  expected key and verifies after download; a second reconcile doesn't re-upload; a profile
  naming an unknown destination is `refused`; an object pre-created with different bytes is
  `conflict`; `lapilli demo` captures stay local.
