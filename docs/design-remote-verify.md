# Design — `kairn verify` on remote objects (v0.2)

Status: **v1, after one loop-engineering round** (log: [`design-review-round7.md`](design-review-round7.md)).

## Problem

Export puts each bundle in a bucket the organization controls (`design-export.md`). To
check that evidence, an auditor or incident reviewer currently has to download it, keep
track of the file, and run `kairn verify` on the copy. Each manual step is a chance to
verify the wrong file. And for evidence in a bucket, "is this file a valid bundle" is only
half the question: the other half is whether the object is still the one Kairn uploaded.

## Goals / non-goals

- **Goal:** `kairn verify s3://bucket/key.ieb` (and `gs://`, and a presigned `https://` URL)
  gives the same verdict as verifying the downloaded file, with the same exit codes.
- **Goal:** a failure to *read* the object never reads as a verdict on the bundle. Network
  errors, 403/404 and size limits are exit 3, never 1 (FAILED) or 0.
- **Goal:** evidence that was replaced or misfiled is caught:
  - a key written more than once;
  - a delete marker followed by a re-upload;
  - a bundle stored under another incident's key;
  - bytes other than the ones the controller recorded.
- **Non-goals:**
  - listing a bucket or verifying many objects;
  - `--output json` (planned; the human output stays unstable, `COMPATIBILITY.md` §2);
  - a GCS generation history.

## Design

### Streamed, never stored

The verifier already streams a `.ieb` in one pass: raw tar over zstd, with no seeking and
no extraction. The object body goes straight into that reader, so nothing is written to
disk. Memory stays bounded by the verifier's own per-file cap (a 300 MB object streamed in
about 17 MB of RSS).

The same reader hashes every byte it passes along. After the tar ends, the rest of the body
is drained, so the output states the SHA-256 of the object exactly as stored. The object's
size from the store must match the bytes read.

A read error from the body (connection reset, timeout) would otherwise look to the tar
reader like a truncated archive, which is FAILED. So the body reader records transport
errors and byte-limit hits on the side. If one happened, the result is **cannot evaluate
(exit 3)**, whatever the tar reader concluded. The compressed body is capped at the
verifier's 2 GiB byte limit.

A mid-stream retry by `object_store` uses a Range request pinned to the same ETag and
version, and validates Content-Range.

### Sources and credentials

| URL | Client | Credentials |
|---|---|---|
| `s3://bucket/key` | `object_store` S3 | `AWS_*` environment: keys and session token, `AWS_REGION`, `AWS_ENDPOINT_URL`, proxy and CA settings, `AWS_ALLOW_HTTP`. Also web identity and container credentials, then instance metadata. |
| `gs://bucket/key` | `object_store` GCS | `GOOGLE_APPLICATION_CREDENTIALS`, or the gcloud application-default file. Supported types are `service_account` and `authorized_user`; after those comes the GCE/GKE metadata server. Workload identity federation (`external_account`) and impersonation files are **not** supported. |
| `https://…` | plain HTTPS GET | none: the URL itself is the credential (a presigned URL) |

`object_store` does not read `~/.aws/config` profiles or SSO caches. When `AWS_PROFILE` is
set and there are no environment credentials, Kairn stops with an error (exit 3) that shows
the command to run: `eval "$(aws configure export-credentials --format env)"`.

`http://` is refused, because evidence must not travel over plaintext. An S3-compatible
test endpoint over HTTP needs `AWS_ALLOW_HTTP=true`, the same explicit opt-in the SDKs use.

### What is printed is what is fetched

- **Bucket names** must follow the naming rules (`[a-z0-9._-]`, 3–222). Otherwise a crafted
  name could become the hostname of a virtual-hosted request and receive the signed request
  (access key id, session token, signature).
- **Keys** are fetched exactly as written. Clients silently normalize a leading `/`, `//`, `.`
  and `..`, so such keys are refused (exit 64) instead of verifying a different object.
- **`https://` URLs** must use a plain `host[:port]`: no user info, backslashes, whitespace,
  escapes or non-ASCII. URL parsers disagree on those, and a disagreement would print one
  host while contacting another. The printed URL never includes the query string.
- **No redirects.** A redirect would carry the presigned credential (via Referer, or to the
  next hop) to a host that is never printed. A 3xx is exit 3.

### Version history (S3)

Kairn writes each key once: a conditional create, never an overwrite. So on a versioned
bucket, a key with more than one version, or with any delete marker, was written again by
someone. Its current bytes may not be the ones Kairn uploaded.

After verifying the object, `kairn verify s3://…` lists every version and delete marker of
exactly that key. It uses S3 ListObjectVersions, which needs `s3:ListBucketVersions`, and
signs the request with `object_store`'s SigV4 signer. The result is:

- **FAILED** if the key has more than one version, has any delete marker, or the fetched
  version is not the newest one. The problem line names the way out: verify each version
  with `--version-id`. The controller records the original's version id (below).
- **Exit 3** if the listing is denied: the history decides whether the object can be trusted.
  `--current-only` skips the check explicitly.
- **Unversioned bucket:** reported as `history=unversioned`, with a note that an overwrite
  there leaves no trace.
- With `--version-id` there is no history check, because that exact version is what is
  being verified.

### Anchors recorded at upload

`status.exports.<name>` records the uploaded object's `sha256` and, on a versioned bucket,
its `versionId`. The auditor can pin both:

```
kairn verify s3://evidence/prod/c1/ic-1.ieb --version-id <versionId> --expect-sha256 <sha256>
```

`--expect-sha256` works for local `.ieb` files too. A mismatch is FAILED.

### Key identity

Kairn's export writes `<prefix>/<cluster_id>/<incident_id>.ieb`. For `s3://` and `gs://`,
the expected cluster and incident default to the key's last two segments, so an object that
holds another incident's bundle is FAILED, just as `--cluster`/`--incident` mismatches are.

- Inference happens only when both segments are ones Kairn writes (`[A-Za-z0-9._-]`, at
  most 100). The controller and the chart refuse to export under any other cluster id.
- Explicit `--cluster`/`--incident` take precedence over the inferred values.
- `--any-key` turns inference off, for copies stored under another layout.
- `https://` paths are never inferred from.

The `identity:` line always says where the expectation came from, or that it was not
checked.

### Output

For a remote object, two lines come before the usual verdict line (human output, not
stable):

```
object: s3://evidence/prod/c1/ic-1.ieb  size=48213  sha256=9f2c…  version=3HL4kqtJ…  history=versions:1,delete-markers:0
identity: cluster=c1 incident=ic-1 (from the object key; --any-key skips)
OK  hash_ok=true context_ok=true coverage=100% …
```

Store errors are collapsed to one line with their error code; no XML bodies are printed.

### Footprint

The remote sources are a default-on cargo feature (`remote`) of `kairn-cli`.

- `--no-default-features` builds a verifier with no network code at all, for people who
  want to audit the smallest binary. CI lints and tests that build.
- The CLI inside the controller image is built that way, so `kubectl exec` can't turn it
  into a bucket reader running with the controller's cloud identity.

## Honest limits

- **GCS has no history check.** `gs://` reports `history=not-checked(not s3)`, and
  `--version-id` is S3 only. For GCS evidence, use a bucket retention policy and compare
  `--expect-sha256` with the recorded value.
- **History can be erased** by anyone allowed to delete object versions
  (`s3:DeleteObjectVersion`), unless the bucket has compliance-mode Object Lock. A key
  rewritten after erasing its history shows 1 version. The recorded anchors
  (`--expect-sha256`, `--version-id`) still catch it.
- **The history is a snapshot.** The GET and the listing are two requests, and a write
  between them shows up as "fetched version is not the newest" (FAILED), never as OK.
- **More than 1000 versions** of one key: the counts are lower bounds (`+`), and the object
  is FAILED anyway.
- **What OK proves.** A successful remote verify says the object's bytes are a valid bundle
  and that the key was written once. Whether the bucket *prevents* rewriting is still the
  organization's configuration (`design-export.md`, "What never overwritten means").
- **The anchors live in the cluster.** `status.exports` is gone with the cluster. Copy the
  sha256 and version id into the incident ticket if they must outlive it.

## Testing

- **Unit tests:**
  - URL parsing: schemes, bucket rules, key normalization, https host spoofing, query
    stripping;
  - key-identity inference;
  - ListObjectVersions parsing (exact key only, delete markers, latest);
  - error collapsing;
  - the body reader's error and limit bookkeeping.
- **Against MinIO**, all 39 conformance fixtures give the same exit code from `s3://` as
  from the local file.
- **kind E2E** (`test/e2e/export.sh`, MinIO with Object Lock):
  - the uploaded bundle is OK;
  - its sha256 and versionId recorded in status match (`--expect-sha256`, `--version-id`);
  - history shows 1 version and the identity comes from the key;
  - the conflicting object, another incident's identity and a wrong sha256 are FAILED;
  - a missing object is exit 3.
