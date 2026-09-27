# `lapilli verify --output json`: `lapilli.dev/verify-result/v1`

Status: **normative, stable from v0.1.0** (docs/COMPATIBILITY.md §2). Review log:
[`docs/design-review-round8.md`](../docs/design-review-round8.md).

`lapilli verify --output json` prints exactly **one JSON document** on stdout and **nothing**
on stderr, for every outcome it could evaluate or failed to evaluate: exit 0, 1, 2 and 3.

- **Usage errors (exit 64)** are text on stderr, with nothing on stdout. A command line that
  doesn't parse may not have asked for JSON at all.
- **Any other exit code** (for example a crash) means there is no result. A consumer checks
  the exit code first, and treats anything other than 0–3 as "no result, reject".
- **If stdout is closed** before the document is written, the exit code is 3.

## Verdict and precedence

`verdict` is one of `OK`, `PARTIAL`, `FAILED`, `CANNOT_EVALUATE`; `exit_code` is 0, 2, 1 or
3 respectively (the same as the process exit code). This set is **closed**.

When several things are known, the verdict is the highest of:

**FAILED > CANNOT_EVALUATE > PARTIAL > OK**

A known failure is never hidden behind "cannot evaluate". For example:

- A bundle whose hashes fail stays FAILED even if the bucket's version history could not
  be read.
- A file that doesn't have the `--expect-sha256` digest is FAILED even if its format is
  unknown.

One exception, stated so it is not mistaken for a promise: when a size or entry **limit**
trips while the archive is still being read, the verifier stops there and reports
CANNOT_EVALUATE with `limit`; structure problems it had already noted in the entries before
that point are not carried into the report. The bundle is over the limit either way, and
nothing inside it has been evaluated.

## Document

Every member listed here is always present. A value that is unknown or was not evaluated is
`null`, never omitted and never a default such as `false`.

| Member | Type | Meaning |
|---|---|---|
| `schema` | string | `"lapilli.dev/verify-result/v1"`. |
| `verifier_version` | string | Version of the tool that verified. |
| `verdict` | string | See above. |
| `exit_code` | integer | See above. |
| `input.type` | string (open) | `file`, `directory`, `s3`, `gs`, `https`. |
| `input.location` | string | The path as given, or the URL. For `https://`, the query string is replaced by `?…`: a presigned URL's credential is never printed. |
| `input.size` | integer \| null | Bytes read, when the whole file or object was read. |
| `input.sha256` | string \| null | Lowercase hex SHA-256 of exactly the bytes that were verified: always for bucket objects; for a local file when `--expect-sha256` is given or the bundle's rules ran. A local stream that isn't a bundle is not read to the byte limit just to report a digest. |
| `input.version_id` | string \| null | For S3: the version that was read, or on failure the version that was requested. |
| `input.history` | object \| null | S3 version history of the key (below). `null` for local input, and when the object could not be read and there was no history to explain why. |
| `expected.cluster` | string \| null | The cluster the bundle had to belong to. |
| `expected.incident` | string \| null | The incident the bundle had to be about. |
| `expected.source` | string (open) | Where the expectation came from: `flags`, `object-key`, `flags+object-key`, `none`. |
| `bundle` | object \| null | What the bundle says about itself. `null` when no Lapilli manifest could be read: unreadable input, the resource limits, no or unparseable `manifest.json`. |
| `bundle.format` | string \| null | `schema_version` as found. |
| `bundle.producer_version` | string \| null | `producer.version`. Non-null exactly when the format's verification rules ran. |
| `bundle.cluster_id`, `bundle.incident_id` | string \| null | The bundle's own identity, when its rules ran. |
| `bundle.hash_ok` | boolean \| null | The hash tree matched every file. `null` if not evaluated. |
| `bundle.context_ok` | boolean \| null | The identity matched `expected`. `null` if not evaluated. |
| `bundle.coverage_score` | number \| null | Fraction of intended collectors that ran, 0–1. |
| `bundle.partial` | boolean \| null | Some intended collectors did not run. |
| `bundle.collectors_run` | array of string \| null | `coverage.collectors_run` as found. |
| `bundle.collectors_intended` | array of string \| null | `coverage.collectors_intended` as found — what `coverage_score` is a fraction *of*. |
| `bundle.deferred` | array of string \| null | `coverage.deferred` as found: collectors the producer declared it did not intend because the data is kept elsewhere (IEB rule 6). Non-empty means **not a full capture** even at `coverage_score: 1`. `[]` when the bundle deferred nothing. |
| `bundle.signature` | string \| null (open) | Summary of the signature check (below). |
| `bundle.redaction_mode` | string \| null (open) | `default`, `strict`, `off`. |
| `problems` | array | Objects with `code` and `message`, in the order found. |

`bundle.*` values are read under the rules of `bundle.format`. A later lapilli that reads
another bundle format reports the same members, with the same meaning.

`bundle.signature` summarizes the signature check:

| Value | Meaning |
|---|---|
| `trusted` | Verified against the caller's `--key`: integrity and producer authenticity. |
| `unpinned` | Self-consistent, but no `--key`, so the signer is not established. |
| `invalid` | Declared or present, and it does not check out. |
| `absent` | Declared unsigned, and there is no signature. |

`trusted` is the only value that establishes the producer. A consumer must treat an
unknown value as not trusted. If more signature kinds come later, `signature` stays the
summary, and a separate array member will carry the details.

`input.history`:

| Member | Type | Meaning |
|---|---|---|
| `state` | string (open) | `listed`, `unversioned` (the store returned no version id), `unavailable` (the listing failed), `not-checked`. |
| `versions`, `delete_markers` | integer \| null | Counts for exactly this key (`listed` only). |
| `fetched_is_latest` | boolean \| null | The version read is the newest listed one. |
| `truncated` | boolean \| null | More than one listing page; the counts are lower bounds. |
| `reason` | string \| null (open) | Why it was not checked: `--version-id`, `--current-only`, `not s3`. |

## Problem codes

`code` is from a set that is only ever extended. `message` is for humans and is **not**
stable. The last column is the verdict the code produces **on its own**; the actual
verdict follows the precedence above.

| Code | Meaning | Alone gives |
|---|---|---|
| `unreadable` | Something needed was not readable: the input (I/O, network, 403/404, a redirect), a `--key` that can't be read or isn't a public key, or a bucket's version history. | CANNOT_EVALUATE |
| `limit` | Over the verifier's resource limits. | CANNOT_EVALUATE |
| `format-unsupported` | A format major this lapilli does not read, or a pre-release format. | CANNOT_EVALUATE |
| `not-a-bundle` | No `manifest.json`, one that isn't JSON, or one without a Lapilli `schema_version`. | FAILED |
| `structure` | The container or the paths break the format's rules: corrupt archive, links or special entries, unsafe, duplicate or case-colliding paths (in the archive or in the hash tree listing), extension records. | FAILED |
| `manifest` | The manifest is malformed or inconsistent: duplicate member names, coverage, collector files, a missing `redaction.json`. | FAILED |
| `integrity` | File contents don't match the hash tree: modified, missing, unexpected, root mismatch. | FAILED |
| `context` | The bundle's cluster or incident is not the expected one. | FAILED |
| `signature` | The bundle's signature: missing, undeclared, invalid, an unknown algorithm, or not by the `--key`. | FAILED |
| `partial` | Some intended collectors did not run. | PARTIAL |
| `notice` | Informational. | nothing |
| `custody` | The store shows the object is not simply the one written: the key was written more than once, has delete markers, or was deleted. | FAILED |
| `digest` | The input's SHA-256 is not `--expect-sha256`. | FAILED |

Routing hints:

- **Evidence was tampered with or replaced:** `integrity`, `structure`, `manifest`,
  `signature`, `custody`, `digest`.
- **Misfiled:** `context`.
- **Operator or environment problem:** `unreadable`, `limit`.
- **Incomplete capture:** `partial`.

## Evolution

Within v1:

- **May:**
  - add members;
  - add values to the members marked *open*;
  - add problem codes.

  Consumers must ignore unknown members, treat an unknown `signature` as not trusted, an
  unknown `redaction_mode` as `off`, and an unknown code as an unclassified problem.
- **Must not:**
  - remove or rename members, values or codes;
  - change a member's type (including making a non-null member nullable);
  - change what a verdict or exit code means;
  - **move a condition to a different code**.

Anything else needs `lapilli.dev/verify-result/v2`, announced a minor release ahead. v1 then
remains available (for example as `--output json-v1`) for at least one more minor release.

## Enforcement

The committed fixtures (`test/fixtures/ieb/<release>/expected.json`) pin each case's exit
code **and** its set of problem codes, including cases about the verifier's inputs:
context, digest, and an unusable `--key`.

The CLI tests run every case in both output modes, and check the document against this
contract: every member, its type, null exactly where the rules didn't run, and codes from
the known set.
