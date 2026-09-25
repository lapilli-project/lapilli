# Bundles signed by a real KMS

Golden data for `docs/design-kms.md`: one bundle per cloud, sealed by the controller against
the real service (not an emulator), with the public key that `lapilli key fetch --kms` returned
at the time. `crates/lapilli-cli/tests/fixtures.rs` verifies each offline with `--key` and
refuses it with another key, so the DER/low-S shape a real KMS produces stays pinned even
after the key is destroyed.

| file | cloud | key (destroyed after the run) | key_id | sealed |
|---|---|---|---|---|
| `gcp-cloudkms-2026-09-25.ieb` | GCP Cloud KMS, `EC_SIGN_P256_SHA256` | `projects/homblabs-a1e67/locations/asia-northeast3/keyRings/lapilli-smoke/cryptoKeys/sign/cryptoKeyVersions/1` | `f4a912bee4dc547a24be00f9492ca34df3b9ae7c2d36f19c157d6208ce84416d` | 2026-09-25, kind v1.37, controller `0.1.0` |

The GCP run authenticated with a bearer token (`GOOGLE_OAUTH_ACCESS_TOKEN` via `extraEnv`); the
Workload Identity credential path was **not** exercised by it. No AWS bundle yet.
