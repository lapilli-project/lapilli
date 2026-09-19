# Design — KMS signing: AWS KMS and GCP Cloud KMS (v0.2)

Status: **v1.1, implemented** (`crates/kairn-kms`, `crates/kairn-controller/src/sealing.rs`; operator guide [`kms.md`](kms.md)); after loop-engineering round 9 (R1 + converge R2) (log: [`design-review-round9.md`](design-review-round9.md)).

## Problem

Static-key signing (v0.1) keeps the private key in a Secret the controller reads. Whoever can
read that Secret, or exec into the controller, can sign anything as Kairn. DESIGN §7 names
KMS as the fix: the key never leaves the KMS, and the controller can only ask for
signatures.

## Goals / non-goals

- **Goal:** sign with an AWS KMS or GCP Cloud KMS asymmetric P-256 key, producing exactly
  what `ieb/v1` specifies:
  - `alg: ecdsa-p256-sha256`, a DER signature, and `key_id` = SHA-256 of the SPKI DER.
  - **No bundle format change and no verifier change.** `kairn verify --key` and cosign work
    as before.
- **Goal:** the admin chooses the key, in the chart. Whoever edits a `CaptureProfile` or
  creates an `IncidentCapture` can neither pick it nor avoid it.
- **Goal:** fail closed. With KMS configured, Kairn never writes an unsigned or wrongly
  signed bundle. A signing outage delays the seal, visibly, and keeps the captured data
  without collecting it again.
- **Goal:** no cloud SDKs. AWS requests are signed with `object_store`'s public SigV4 signer
  and credential chain (IRSA, EKS Pod Identity, env). GCP uses `object_store`'s token
  provider (Workload Identity, service-account files; scope `cloud-platform`).
- **Non-goals:**
  - Azure Key Vault, PKCS#11, keyless/Sigstore (v0.3);
  - automated key rotation;
  - a `/metrics` endpoint (the controller has none today; tracked separately).

## Prerequisite fixed first: who can get a bundle vouched for

A sealed bundle names its cluster and incident, and a signed one vouches for them. Before
round 9, the seal path trusted `IncidentCapture.spec`: anyone who could create a capture
could get a (signed) bundle for **another cluster**, and an unsafe `incidentId` became a
file path, a traversal in the staging directory's cleanup. That affected v0.1 static
signing too. Fixed, independently of KMS:

- **Refused captures.** The controller refuses any capture whose `spec.clusterId` isn't its
  own (`cluster-mismatch`), whose `incidentId` isn't `[A-Za-z0-9._-]{1,100}`
  (`invalid-incident-id`), or that uses the webhook's id form `<cluster>-<16 hex>` under
  another name than `ic-<hex>` (`reserved-incident-id`). The cluster id itself must be
  `[A-Za-z0-9._-]{1,83}`: it's enforced at controller start and in the chart schema.
- **An atomic claim per incident id.** Before touching anything, a capture claims its
  incident id by creating `<incident>.ieb.owner` exclusively (`O_EXCL`), holding its UID.
  Another capture with the same id is refused (`incident-id-in-use`), whether the first one
  is collecting, sealing, done or failed.
  - A retry of the same capture finds its own claim and adopts its finished bundle.
  - Staging directories and temporary files are named per UID.
  - A bundle is renamed into place only by the claim holder.
- **One bundle root.** Bundles are written only under the controller's `KAIRN_BUNDLE_ROOT`.
  A profile whose `export.path` differs is refused (`export-path-not-allowed`): a claim is
  only exclusive within one directory.
- **E2E.** All refusals are tested, with the original bundle byte-identical.

## Design

### Configuration (admin only)

```yaml
signing:
  mode: kms                  # none | static | kms
  kms:
    # AWS: a key ARN, arn:<partition>:kms:<region>:<account>:key/<key-id>. Alias ARNs are
    #      refused: an alias can be repointed to another key by UpdateAlias, and IAM can't
    #      be scoped to one.
    # GCP: a key *version*, projects/<p>/locations/<l>/keyRings/<r>/cryptoKeys/<k>/cryptoKeyVersions/<n>
    key: ""
```

- **What the controller does.** The chart passes `--signing-mode` and `--signing-kms-key`,
  and the provider follows from the key's syntax. With `mode: kms`, **every** bundle is
  signed with that key.
- **Profiles can't change it.** Their `signing` field is ignored, and the capture's status
  says so. No CRD enum changes: with `mode: kms`, the chart renders its own profile with
  `signing.mode: none`, and the schema allows `kms` at the chart level only.
- **Rollback.** A v0.1 controller doesn't know `Sealing`: it would re-collect such captures
  and seal them with the profile's signing. Before rolling back, wait until no capture is
  in `Sealing` (the upgrade notes say how to check).
- **Schema validation.** The chart schema requires `kms.key` to match one of the two
  patterns.
- **Regions.** They come from the ARN, and the partitions `aws`, `aws-cn` and `aws-us-gov`
  map to their KMS endpoint hosts.
- **Test endpoints.** They come only from `AWS_ENDPOINT_URL_KMS` / `KAIRN_GCP_KMS_ENDPOINT`,
  and are logged loudly at startup. Plain HTTP is allowed only to loopback and cluster-local
  names. Setting them requires editing the Deployment, which already means owning the
  controller.

### Preflight and identity pinning

At startup, and again until it succeeds, the controller:

- **Fetches the public key** (`GetPublicKey` / `getPublicKey`) and checks it:
  - AWS: `KeySpec=ECC_NIST_P256`, `KeyUsage=SIGN_VERIFY`, and a response `KeyId` equal to the
    configured key ARN.
  - GCP: `algorithm=EC_SIGN_P256_SHA256`, and a response `name` equal to the configured
    version.
  - Both: the returned SPKI parses as P-256.
- **Computes `key_id`** exactly as for static keys, and logs it with the key name.
- **Stays ready.** It never gates readiness on KMS: an unready pod leaves the Service and
  Alertmanager's webhook calls would fail, losing captures, the opposite of fail-closed.
  - A preflight failure is logged and emitted as a pod Event.
  - It is retried with backoff, and captures meanwhile collect and wait in `Sealing`.
  - A definitive misconfiguration (access denied, wrong key spec, identity mismatch) is
    reported loudly, but still doesn't crash the pod: collected data is worth more than a
    fast failure.
  - It cannot prove `Sign` permission without signing; the E2E and the first seal exercise
    it.

### Sealing as its own phase

Today, collect, seal and pack run in one step, and any error is final. With KMS, sealing
becomes a phase of its own:

1. **Collect and prepare.** Collect into `.staging-<incident>`, then `prepare_seal` writes
   `manifest.json`, whose signing declaration names the KMS `key_id`. The capture's status
   becomes `Sealing` with `seal = {attempts, nextAttemptAt, reason}`. From here on:
   - spec edits are ignored, as for `Exported`;
   - the generation is pinned;
   - nothing is collected again.
2. **Sign and attach.** On each attempt, which re-runs the capture refusals first:
   - Recompute the hash tree of the staging directory, and check that it equals the one in
     the existing `manifest.json`. The manifest bytes are read from the file and never
     rebuilt (`sealed_at` is fixed), and never taken from status, which anyone allowed to
     patch it controls.
   - Require the recorded `incident.cluster_id` = this controller's and `incident.id` = the
     capture's. A self-consistent staging directory written by someone else can't get a
     signature for another cluster.
   - The manifest is rebuilt on each attempt from what was recorded at collection (same
     input and tree, so the same bytes), declaring the **currently pinned** key. If the admin
     changed the key while a capture waited, it is signed with the new key, and
     `status.seal.key` / `keyId` say so.
   - A missing staging directory is terminal (`staging-lost`, Failed), not retried.
   - Ask KMS to sign `SHA-256(manifest bytes)`:
     - AWS: `Sign` with `MessageType=DIGEST` and `SigningAlgorithm=ECDSA_SHA_256`; the
       response `KeyId` must be the pinned ARN.
     - GCP: `asymmetricSign` with `digest.sha256` and `digestCrc32c` (int64 as JSON
       strings); the response must have `verifiedDigestCrc32c=true`, a matching
       `signatureCrc32c`, and `name` equal to the pinned version.
   - Parse the DER and normalize it to low-S, for canonical output: `ieb/v1` producers emit
     low-S, and KMS doesn't promise it. Then **verify locally** against the pinned public
     key before calling `attach_signature`. A signature that doesn't verify is an error,
     never a bundle.
   - Pack, and continue as today (`Exported`, then object-store export).
3. **Retry and fail.** Retries use the export backoff (30 s … 1 h, 24 attempts), requeued
   by the reconcile loop, not by the 10 s error policy.
   - **Events.** On the first failure, and when attempts run out.
   - **Reasons.** `signing-unavailable` (network, throttling), `signing-denied`
     (permission), `signing-key-changed` (preflight identity mismatch).
   - **After the last attempt.** The capture is `Failed`, and the staging data **is kept**.
     The annotation `kairn.dev/retry-seal=<any new value>` starts another round of attempts
     without collecting again.
4. **Across restarts.** The Deployment is `Recreate`, so every upgrade restarts it. A
   capture in `Sealing` resumes from step 2 after a restart. The E2E restarts the
   controller during a KMS outage.

The staging directories of captures waiting to seal use PVC space. The chart NOTES warn
when `signing.mode=kms` and the PVC is at its default 1 GiB. A staging budget is out of
scope for v0.2.

### Least privilege and custody (required configuration)

**AWS.** Use a key dedicated to Kairn, never shared, for example with cosign image
signing. IAM for the controller role:

```json
{ "Statement": [
  { "Effect": "Allow", "Action": "kms:Sign", "Resource": "<key ARN>",
    "Condition": { "StringEquals": { "kms:SigningAlgorithm": "ECDSA_SHA_256",
                                      "kms:MessageType": "DIGEST" } } },
  { "Effect": "Allow", "Action": "kms:GetPublicKey", "Resource": "<key ARN>" } ] }
```

The two actions are in separate statements because the condition keys exist only on
`Sign`/`Verify`: on `GetPublicKey` the condition never matches, and the allow would never
apply.

The **key policy** must not leave `kms:Sign` open to the account through the default
root-delegation statement. Either list principals explicitly, or add
`Deny kms:Sign unless aws:PrincipalArn = <controller role>`. Otherwise any IAM principal
granted `kms:Sign` can forge bundles offline.

**GCP.**

- Grant `roles/cloudkms.signer` and `roles/cloudkms.publicKeyViewer` on the one key, to the
  controller's Workload Identity principal.
- Enable **Data Access audit logs** (`DATA_READ`) for `cloudkms.googleapis.com`: without
  them, signatures are not logged at all.

**Matching signatures to audit logs.** For each signature, the controller logs, and records
in status, the manifest digest and the cloud request id. This lets an auditor match a
bundle to a CloudTrail or Cloud Audit Logs entry. CloudTrail's `Sign` event records the
key and algorithm, not the digest.

**Getting the public key.** `kairn key fetch --kms <key>` writes `kairn.pub` and prints its
`key_id`. It uses the same credential chain as `kairn verify s3://`, and is the trust
anchor: it asks the KMS itself.

Planned, not in this release: the controller writing `keys/<key_id>.pub` next to the
bundles and to every export destination (append only), so the key for an old bundle
survives key disablement. Until then, the rotation runbook says to keep the old public key.
When it lands, it is for **availability, not trust**: anyone who can write the bucket can
put a key there, so the key must still be one the auditor was given or fetched from KMS.
`kairn verify` never picks a key from the bundle's own storage.

**GCP grants.** `cloudkms.signer` inherited from the key ring, project or folder also allows
signing: audit those, not only the key's own policy.

**Rotation.** Asymmetric AWS keys don't auto-rotate. Rotation means:

1. Create a new key and grant it.
2. Keep the old public key (`kairn key fetch`, before disabling the old key).
3. Update `signing.kms.key` in the chart.

The runbook lives in `docs/kms.md`.

### What KMS does and doesn't prove (honest)

- **What it gives.** Stealing the controller's Secret or filesystem no longer gives a
  signing key.
- **Who can still sign.** It does **not** stop these from getting signatures over fabricated
  manifests:
  - anyone the key policy (and, on GCP, inherited IAM) lets call `kms:Sign`;
  - a compromised controller while it holds the role;
  - anyone who can run a pod as the controller's ServiceAccount (pod-create rights in its
    namespace).
  KMS narrows who can sign to what those policies say.
- **Matching logs to bundles.** The control that catches forgery is the **reverse match**:
  every `Sign` event in CloudTrail or Cloud Audit Logs for this key must correspond to a
  bundle the controller recorded. An unmatched event is a forgery signal. The recorded
  digest doesn't prove anything by itself: a compromised controller can record a false
  one, and a direct `kms:Sign` records nothing.
- **One key per cluster.** Recommended, with the auditor mapping `key_id` to cluster. With
  one key serving several clusters, the refusal above is the only thing keeping one
  cluster's controller from vouching for another.
- **Signing time** is still self-asserted.

## Testing

- **Unit:**
  - request shapes (SigV4 for `kms`, the GCP JSON with CRC32C as strings);
  - key-name parsing (key ARN yes, alias no; partitions; GCP version path);
  - response identity checks;
  - DER parsing, and low-S normalization (`normalize_s()` returns `None` when already low,
    so use `unwrap_or`), asserting `!s.is_high()` after normalization;
  - rejection of a signature from another key. Not a high-S signature: p256 accepts those.
- **AWS:** LocalStack, pinned to a tag that runs without an auth token and implements
  ECC_NIST_P256 `DIGEST` signing (confirmed before adopting it). Tested flows:
  - seal and `kairn verify --key` with the key from `kairn key fetch`;
  - stop LocalStack, then restart the controller: the capture stays `Sealing` and no bundle
    appears; restore LocalStack, and the bundle seals, still verifies, and nothing was
    collected again.
- **GCP:** an open-source KMS REST emulator (floci-gcp or gcp-kms-emulator) if it runs
  offline, so the wire format isn't only our own reading of it. Otherwise a local fake, with
  that limitation stated.
- **Real clouds:** a maintainer runs the quickstart on AWS and GCP before tagging (a
  `RELEASE.md` item), and keeps one real request/response pair per cloud as golden test
  data.
