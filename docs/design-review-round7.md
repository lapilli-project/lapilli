# Design Review — Round 7 (`kairn verify` on remote objects)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
[`design-remote-verify.md`](design-remote-verify.md) and its implementation (snapshot: the
working-tree diff `0fbd3d97d094`, frozen while the critics ran).

**Outcome: time-boxed, not dry.** One round. Every finding was APPLIED or given an explicit
disposition. The fixes are covered by unit tests, the MinIO runs and the kind E2E, but no
second critic round attacked them.

## Round 0

- **Category:** security-sensitive and public. This is the path auditors use to decide
  whether stored evidence can be trusted. It handles cloud credentials and presigned URLs.
- **Break-even:** cost ≈ 3 critic runs. The downside is a false OK on replaced evidence, a
  leaked credential, or a verdict that doesn't match the local verifier. Downside ≫ cost.
- **Sizing:** medium, 3 lenses × 1 round, time-boxed like rounds 4–6. The security lens was
  mandatory.
- **Independence: not achieved (calibration only).** All critics were Claude. Their claims
  were checked against the `object_store` 0.14.2 / reqwest 0.13.5 sources and live MinIO
  runs. The human or non-Claude review gate in `RELEASE.md` still applies.

## Findings and dispositions

### Auditor lens

| # | Sev | Finding | Disposition |
|---|---|---|---|
| A1 | BLOCKER | Only the current version was checked. An overwritten object verified cleanly, and the "versioned bucket" note printed the same text whether or not the key had history. | **APPLY.** ListObjectVersions for the exact key, signed with `object_store`'s public `AwsAuthorizer`. A key with more than one version, any delete marker, or a fetched version that isn't the newest is FAILED. A denied listing is exit 3, unless `--current-only`. Verified live: overwrite → FAILED; delete marker plus re-upload → FAILED. |
| A2 | MAJOR | Key-identity inference silently skipped some layouts and gave a FAILED on legitimate ones, indistinguishable from tampering. | **APPLY (partly).** A stdout `identity:` line always says where the expectation came from, or that nothing was checked. A new `MISFILED` verdict: **REFUTED**, since the frozen exit codes can't grow one, and the problem line plus the `identity:` line already tell a misfiled object from tampering. Requiring flags when nothing can be inferred: **not applied**; the `identity:` line states "not checked" instead. |
| A3 | MAJOR | Nothing is stably parseable; XML error dumps. | **APPLY:** errors collapse to one line with their code, and COMPATIBILITY says remote read failures are exit 3. `--output json`: **VALID-OUT-OF-SCOPE** (already planned, not part of v0.1). |
| A4 | MAJOR | AWS profiles and SSO fail with a baffling IMDS error; the workaround was documented only in the design doc. | **APPLY.** Stops early with the `export-credentials` command when a profile is set without env credentials. Documented in the README and `--help`. |
| A5 | MAJOR | Nothing to compare the printed SHA-256 against; status recorded neither hash nor version. | **APPLY.** The controller records `status.exports.<name>.sha256` and `versionId`. Added `--expect-sha256`, which also works for local files. Reading the store's own checksum: **REFUTED**, since whoever overwrites the object sets that checksum too. |

### Security lens

| # | Sev | Finding | Disposition |
|---|---|---|---|
| S1 | MAJOR | `https://evil\@trusted/…` contacted `evil` but printed `trusted`: the split in `display()` disagreed with WHATWG. | **APPLY.** Strict `host[:port]` allowlist, and no backslashes, whitespace, userinfo, escapes or non-ASCII (exit 64). The printed authority is the validated one. |
| S2 | MAJOR | Redirects were followed, carrying the presigned query in Referer to the next host. | **APPLY.** `Policy::none()` and `referer(false)`; a 3xx is exit 3. The version listing doesn't follow redirects either. |
| S3 | MINOR | Unvalidated bucket names could steer a virtual-hosted signed request to another host. | **APPLY:** bucket naming rules, exit 64. |
| S4 | MINOR | `s3://b//k` fetched `k` but printed `//k`. | **APPLY** (see C1). |

### Correctness lens

| # | Sev | Finding | Disposition |
|---|---|---|---|
| C1 | MAJOR | A leading `/` (and `Path::parse` normalization) verified a different object. | **APPLY.** Empty, `.` and `..` segments are refused at parse time (exit 64), and before the GET the parsed path must equal the key. |
| C2 | MAJOR | A cluster id with `/` or characters that get percent-encoded makes genuine exports FAILED under inference. | **APPLY.** The controller refuses to export under a non-`[A-Za-z0-9._-]` cluster id (`invalid-cluster-id`), and the chart fails the render. The CLI infers only from path-safe segments. |
| C3 | MINOR | `with_client_options` dropped proxy, CA and other client settings from the environment; `AWS_ALLOW_HTTP=1` was rejected. | **APPLY.** Client options are rebuilt from the `AWS_*` variables through `object_store`'s own key parser, then the timeouts are set. |
| C4 | MINOR | A small CANNOT-EVALUATE object (e.g. format v0) lost its `object:` line and verdict line. | **APPLY.** Only limit hits stop early; any other verdict is printed as it is locally. |
| C5 | MINOR | The GCS credential row claimed federation and impersonation files, which aren't supported. | **APPLY:** the doc lists what is and isn't supported. |

## Beyond the critics

The controller image's in-pod `kairn` is now built with `--no-default-features`. With exec
access, the remote-enabled CLI would otherwise read buckets with the controller's cloud
identity. CI lints and tests that build.

## Verdict

**Time-boxed, not dry.** All BLOCKER and MAJOR findings are APPLIED, except the two
partial rejections recorded above (A2's new verdict, and A5's store checksum). The version
history check (A1) is the largest new surface. It is exercised against MinIO:

- a clean key;
- an overwritten key;
- a delete marker then a re-upload;
- a pinned version;
- `--current-only`;
- an unversioned bucket.

It was not re-attacked by a second critic round. Two known gaps are recorded in the design
doc: GCS has no history check, and history can be erased without Object Lock.
