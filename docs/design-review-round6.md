# Design Review — Round 6 (object-store export)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
[`design-export.md`](design-export.md) and its implementation. Outcome: **time-boxed, not
dry**. The R2 findings were fixed and each is covered by the kind E2E, but they were not
re-attacked by another critic round.

## Round 0

- **Category:** a security-sensitive surface. DESIGN §7 names the export path as the
  "sanctioned exfil channel", and bundles hold unredacted logs.
- **Break-even:** cost ≈ 4 critic runs. Downside: turning the product into an exfiltration
  tool, or silently losing evidence custody. Downside ≫ cost.
- **Sizing:** medium, 3 lenses × 2 rounds. The security lens was mandatory.
- **Independence: not achieved (calibration only).** All critics were Claude. API facts
  were checked in the `object_store` 0.14.2 source and against a real MinIO with Object
  Lock.

## R1 — design v0, three lenses

- **BLOCKER (security; adopter agreed independently): the allowlist matched only the URL.**
  A profile could keep an allowed bucket name and point `endpoint` at an attacker's server.
  Fixed by design: the admin defines whole destinations (url, endpoint, region, TLS,
  credentials) in the chart, and profiles reference them **by name only**.
- **Retries could never run.** The phase machine returned `await_change` as soon as a
  capture was `Exported`. Now seal → `Exported` → upload from the local file, and
  reconcile requeues until every destination is settled.
- **A JSON merge patch replaces arrays**, so `status.exports` became a map keyed by
  destination.
- **Integrity and key squatting.** The upload now sends a server-verified SHA-256 on S3, an
  existing object is downloaded and hashed, and the key uses the controller's cluster id.
- **API facts corrected from source:** `head()` returns no attributes. Conditional create
  works on AWS, GCS and MinIO. The TLS feature was being unified implicitly.
- **Wording:** "never overwritten" and WORM were overclaimed. The limits are now stated,
  with compliance-mode Object Lock, a bucket policy requiring `if-none-match`, and a
  least-privilege IAM policy.
- **Adoption:** IRSA/Workload Identity annotations were impossible under the values schema;
  a destinations list avoids a breaking change later; demo captures stay local; Events and
  the object URL are recorded in status.

## Arbiter pre-emption (before R2 reported)

A user allowed to patch `status` could set `bundlePath: /etc/passwd` with a pending export,
and the controller would upload that file. The fix is that **only a verified `ieb/v1`
bundle of this capture** (matching cluster and incident) is uploaded. The E2E reproduces the
forged status and asserts `refused (not-a-verified-bundle)` with nothing in the bucket.

## R2 — the implementation

The earlier fixes held: admin-only destinations, the cluster-id key, the probe, chart RBAC,
and the forged-status gate. New findings, all APPLY:

1. **MAJOR: a transient startup failure disabled a destination for good.** An unsynced
   Secret made every capture `refused`. Now only configuration errors are permanent; clients
   and credentials are built at first use and retried (`credentials-unavailable`).
2. **MAJOR: a forged "local" alert could claim the real capture.** The webhook has no auth,
   and the real alert then got a 409, so the capture never left the PVC. The export mode is
   now part of the capture's identity, and local captures show `local-only`.
3. Merge patches couldn't clear stale fields. Unset fields are now serialized as null.
4. A spec edit re-captured an exported bundle and made it conflict with its own upload. A
   capture is now captured once.
5. Other fixes:
   - compare the size before downloading an existing object;
   - move verify and read off the async runtime;
   - classify errors correctly (`NotImplemented` → refused);
   - the chart refuses destinations with the default `clusterId`;
   - no GCS checksum claim;
   - the "no re-upload" E2E now asserts `lastAttemptAt` is unchanged, even across a spec edit.

## Verdict

Time-boxed, not dry. R2 was the last round of the medium budget. Its fixes are exercised by
the E2E (MinIO with Object Lock: exact bytes, no second attempt after a poke and a spec edit,
unknown destination refused, conflict with the original intact and an Event, forged status
refused, demo captures `local-only`), not by another critic. The webhook itself is still
unauthenticated. That predates this work, and an optional shared-secret or NetworkPolicy
for it is the natural next hardening item.
