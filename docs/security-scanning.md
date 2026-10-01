# Scanning the Lapilli image

If you run Trivy, grype, Harbor or ECR scanning against `ghcr.io/lapilli-project/lapilli-controller`,
this page says what you should expect to see, why the base images are pinned the way they are, and
what Lapilli's binaries actually link. It is meant to be checkable: every claim below has a command
next to it.

## Reproduce the scan

```sh
# whatever you use; grype shown because it names the fix version
grype ghcr.io/lapilli-project/lapilli-controller:<version>
trivy image ghcr.io/lapilli-project/lapilli-controller:<version>
```

Findings come from the base image's Debian packages, not from Lapilli's code: the image is
`gcr.io/distroless/cc-debian13:nonroot` plus two statically-linked-except-for-libc Rust binaries.
Its fourteen Debian packages are the whole attack surface a package scanner can see.

## The base images, and why they are pinned by digest

`Dockerfile` pins both stages by digest:

| stage | image | why |
|---|---|---|
| build | `rust:1-trixie@sha256:a8a5f0a1…` | glibc 2.41 |
| runtime | `gcr.io/distroless/cc-debian13:nonroot@sha256:54df941e…` | glibc 2.41, `nonroot` (uid 65532) |

Two rules govern these two lines:

1. **Same Debian release on both sides.** The build glibc must not be newer than the runtime's or
   the runtime cannot load the binary. This is not hypothetical: an earlier revision used
   `rust:1-slim`, which moved from bookworm to trixie on its own, against a `cc-debian12` runtime,
   and produced binaries that would not start.
2. **Digest, not tag.** A tag like `:nonroot` is rebuilt in place. Pinning the digest means what
   ships is the image that was scanned and E2E-tested, and that a change to it is a commit.
   `.github/dependabot.yml`'s `docker` entry opens the PR that moves the pins weekly; bump both in
   the same PR, keep them on the same Debian release, and re-run `test/e2e/run.sh`.

### Why trixie and not bookworm

`gcr.io/distroless/cc-debian12:nonroot` reports two Critical findings that cannot be fixed by
refreshing the tag:

```
libssl3   3.0.20-1~deb12u2   fixed in 3.0.22-1~deb12u1   CVE-2026-75803   Critical
libc6     2.36-9+deb12u14    won't fix                   CVE-2026-5450    Critical
```

The libssl3 fix version, `3.0.22-1~deb12u1`, does not exist in Debian bookworm, so no base-image
refresh clears it; the libc6 one is marked "won't fix" upstream. `cc-debian13:nonroot` carries
`libssl3t64 3.5.7-1~deb13u2` and `libc6 2.41-12+deb13u4`, and reports **no Critical at all** — its
worst findings are three High (two in `libc6`, one in `zlib1g`).

Neither Critical was ever *reachable* from Lapilli's binaries (see the next section). The move was
made anyway, because a Critical in an adopter's scan is a real cost whether or not it is
exploitable: CRITICAL-blocking admission and registry policies refuse the image, and asking every
adopter to read a VEX statement before they can install is worse than not having the package.

## What the binaries link

Both binaries are dynamically linked against three libraries and nothing else — no libssl, no
libcrypto, no `RUNPATH` that could redirect them:

```console
$ readelf -d lapilli-controller | grep -E 'NEEDED|RUNPATH|RPATH'
 0x0000000000000001 (NEEDED)  Shared library: [libgcc_s.so.1]
 0x0000000000000001 (NEEDED)  Shared library: [libm.so.6]
 0x0000000000000001 (NEEDED)  Shared library: [libc.so.6]
$ readelf -d lapilli                | grep -E 'NEEDED|RUNPATH|RPATH'      # identical
$ readelf --dyn-syms lapilli-controller | grep -cw dlopen
0
```

No `dlopen` means the list above is the whole set: nothing can be loaded later. And nothing in the
dependency graph wants OpenSSL in the first place:

```console
$ grep -E '^name = "(openssl|openssl-sys|native-tls)"' Cargo.lock    # no output
$ cargo deny --config deny.toml check bans                            # bans openssl-sys and native-tls
bans ok
```

TLS is rustls with the **ring** crypto provider, installed explicitly at startup, compiled into the
binary. `deny.toml` bans `openssl-sys`, `native-tls`, `aws-lc-rs` and `aws-lc-sys` so that a
dependency bump cannot change this quietly — which it once did: rustls 0.23's *default* features
include `aws_lc_rs`, and the `0.1.0-rc.1` controller shipped a statically linked aws-lc (a
BoringSSL/OpenSSL derivative) alongside ring because one manifest line was missing
`default-features = false`. That is fixed and now enforced by the ban, not by a comment.

The practical consequence: a *package* scanner reporting `libssl3`/`libssl3t64` against this image
is reporting a file in the base layer that no Lapilli process opens. As of 2026-10-01 that is three
High findings — `CVE-2026-72897`, `CVE-2026-84782`, `CVE-2026-84784` — and they are in the table
below for exactly that reason: counted, because a scanner counts them, and unreachable. A *binary* scanner that
reports OpenSSL-like symbols inside `lapilli-controller` is looking at ring, or — in `0.1.0-rc.1`
only — at the statically linked aws-lc described above.

## What remains, and its status

Measured with grype on the published images, as `RELEASE.md` step 9 requires — the released image
itself, not a prediction from the same source. **A row is only comparable with another row scanned
on the same day**, because the count moves when the vulnerability database learns something, not
only when the image changes:

| image | scanned | Critical | High | Medium | Low | Negligible |
|---|---|---|---|---|---|---|
| published `0.1.0-rc.1` (bookworm base) | 2026-09-28 | **2** | 6 | 11 | 2 | 12 |
| published `0.1.0` (trixie base) | 2026-09-28 | **0** | 3 | 10 | 2 | 7 |
| published `0.1.0` (trixie base) | 2026-10-01 | **0** | 6 | 13 | 6 | 7 |
| **published `0.2.0`** (trixie base) | 2026-10-01 | **0** | 6 | 13 | 6 | 7 |

The last two rows are the point of including both. `0.2.0` shows twice the High of `0.1.0` — and
re-scanning `0.1.0` on the same day gives **exactly the same six**, down to the CVE ids. The image
did not get worse; the database learned three `libssl3t64` advisories in the three days between.
Reading the release as the cause would have been the obvious mistake, and it is why this table now
carries a date per row rather than one date above it.

The six High on 2026-10-01: `CVE-2026-19499` and `CVE-2026-5435` in `libc6`, `CVE-2026-85091` in
`zlib1g`, and `CVE-2026-72897`, `CVE-2026-84782`, `CVE-2026-84784` in **`libssl3t64`** — a package
this page did not account for until now, which is the condition `RELEASE.md` step 9 names as needing
action rather than a note.

**`libssl3t64` is present but not linked** — *What the binaries link* above already establishes
that, and now has the CVE ids attached to it. That is an argument about reachability,
not that the finding is wrong: a scanner reports what is installed, and an adopter's policy may
refuse the image on the count alone.

Everything that remains is in `libc6`, `zlib1g` and `libssl3t64`.

- **`libc6`** is genuinely linked (`libc.so.6` above). Its findings are Debian "won't fix" or
  unfixed-in-trixie glibc issues; the remedy when Debian ships one is a base-image digest bump,
  which is what the weekly Dependabot PR is for.
- **`zlib1g`** is present in the base layer but not in either binary's `NEEDED` list, and
  `Cargo.lock` has no `libz-sys`, `libz-ng-sys`, `flate2` or `zlib-rs` (compression in a bundle is
  zstd). Nothing in the image loads it.

If your policy needs a machine-readable statement for the second case rather than this prose, the
following is accurate for any version built from this `Dockerfile` — substitute the image digest
you are scanning:

```json
{
  "@context": "https://openvex.dev/ns/v0.2.0",
  "@id": "https://github.com/lapilli-project/lapilli/.well-known/vex/zlib1g",
  "author": "Lapilli maintainers",
  "statements": [
    {
      "vulnerability": { "name": "CVE-2026-85091" },
      "products": [
        { "@id": "pkg:oci/lapilli-controller?repository_url=ghcr.io/lapilli-project@sha256:<digest>" }
      ],
      "status": "not_affected",
      "justification": "vulnerable_code_not_present",
      "impact_statement": "zlib1g ships in the distroless base layer but is not in either binary's ELF DT_NEEDED list, the binaries contain no dlopen, and Cargo.lock has no libz-sys/libz-ng-sys/flate2/zlib-rs dependency. No Lapilli process loads libz."
    }
  ]
}
```

This project does not yet publish signed VEX documents as release artifacts; the paragraph above is
the claim, and the commands in this file are how you check it yourself.

## What this page does not claim

- The image carries **BuildKit SBOM and provenance attestations** (`provenance: mode=max`,
  `sbom: true`). Those are unsigned in-toto attestations in the OCI index, readable with
  `docker buildx imagetools inspect … --format '{{ json .Provenance }}'`. They are *not*
  Sigstore-signed SLSA provenance, and nothing verifies a signature over them.
- The **CLI tarballs and `SHA256SUMS`** are different: each carries a Sigstore-signed
  build-provenance attestation, checkable with `gh attestation verify`. See
  [README § Verifying what you downloaded](../README.md#verifying-what-you-downloaded).
- Nothing here is a statement about vulnerabilities in Lapilli's own code. For that, see
  [`SECURITY.md`](../SECURITY.md); a verifier-focused review record is in
  [`docs/independent-review-log.md`](independent-review-log.md).
