# Independent review brief: the bundle verifier

`RELEASE.md` requires, before the first release, a review of the verifier of untrusted input
by **a human or a non-Claude model**. Every design round so far (rounds 1–19 under `docs/`)
used Claude critics only, so they share blind spots — which is the whole reason this brief
exists. Later rounds repeatedly found defects in earlier rounds' own fixes, which is calibration,
not independence. This brief is everything a reviewer
needs; it assumes no prior knowledge of Lapilli.

## What Lapilli's verifier promises

`lapilli verify <bundle>` reads an Incident Evidence Bundle (`.ieb`: ustar + zstd), possibly
produced by an attacker, and returns a verdict:

| Exit | Verdict | Meaning |
|---|---|---|
| 0 | OK | Every file matches the manifest's hash tree, the context matches what the caller expected, every intended collector ran, and a declared signature is valid (and trusted if `--key` was given). |
| 2 | PARTIAL | As OK, but some intended collectors did not run. |
| 1 | FAILED | Anything else about the bundle is wrong. |
| 3 | CANNOT_EVALUATE | Unreadable input, over the resource limits, or an unknown format major. |
| 64 | usage error | |

The normative rules: [`spec/IEB-SPEC.md`](../spec/IEB-SPEC.md) (format and verification) and
[`spec/VERIFY-RESULT.md`](../spec/VERIFY-RESULT.md) (the JSON result and its problem codes).

## Scope (in priority order)

1. `crates/lapilli-bundle/src/verify.rs`:
   - `read_ieb_from`: raw tar entries over zstd, single pass, never extracts.
   - `Contents::add_hashed` and `Contents::add_dir`: duplicates, case folding, file/dir
     conflicts, reserved names.
   - `read_dir` / `walk`: an unpacked directory.
   - `evaluate`: format dispatch, `NoDuplicateKeys`, manifest parse.
   - `v1`: hash tree, coverage, redaction record, context, signature.
2. `crates/lapilli-bundle/src/hashtree.rs` (`check_path`, `ustar_representable`,
   `case_collisions`, root computation), `manifest.rs`, `sign.rs` (`key_id`, `verify_b64`).
3. `crates/lapilli-cli/src/remote.rs` `Body`: the reader that hashes, counts and bounds bytes
   fed to the verifier, and records transport errors so that they aren't verdicts.
4. `crates/lapilli-cli/src/verify_cmd.rs`:
   - `run`: argument handling, `--key` parsing, verdict precedence.
   - `verify_local`: a single read.
   - `to_json`.
5. `crates/lapilli-bundle/src/pack.rs` `unpack` (extraction for humans; same rules).

Out of scope: the controller, collectors, the Helm chart.

## Threat model

The attacker controls the bytes of the bundle, including every tar header, the zstd frames
and the JSON. They may also control the object store serving it, but not the verifier's
machine or the `--key` file.

The attacker wins with any of:

- **OK or PARTIAL for bytes the producer never sealed:**
  - a modified, added or removed file that isn't detected;
  - a signature that passes `--key` without the private key;
  - a manifest the verifier reads differently from another conforming reader.
- **A different verdict for the same bytes**, depending on:
  - whether the bundle is verified packed (`.ieb`) or unpacked (a directory);
  - text vs JSON output;
  - local vs remote reading.
- **Resource exhaustion** beyond the stated limits:
  - 100k entries;
  - 2 GiB total;
  - 16 MiB for files read into memory;
  - zstd window.
- **A path outside the destination** on `lapilli unpack`.
- **A remote read failure presented as a verdict on the bundle**, or the reverse.

## Invariants to try to break

- A duplicate path is always FAILED, including `manifest.json` twice and paths that differ
  only by case. So is a path that is both a file and a directory.
- Only plain ustar is accepted. pax and GNU records, links, and special entries are all
  FAILED. Sizes are never taken from extension records, and entry types are checked before
  sizes.
- The hash tree covers every file except `manifest.json` and `signature/*`; `signature/`
  may contain only the two known files, plus `ext/`, which is ignored.
- JSON member names are unique at every depth. `schema_version` is dispatched before any
  other rule is applied.
- `--key` is the only source of authenticity. The embedded public key only proves
  self-consistency, and a declared signature can't be stripped silently.
- The same bundle gives the same verdict packed and unpacked (the fixtures test this for 39
  cases).
- With `--output json`, what is hashed (`input.sha256`) is exactly what was verified.

## How to run things

```sh
cargo test --workspace                     # unit tests + the fixture contract
cargo run -p lapilli-cli -- verify test/fixtures/ieb/v0.1.0/ok-unsigned.ieb --output json
python3 test/spec/build_from_spec.py /tmp/b && cargo run -p lapilli-cli -- verify /tmp/b
scripts/release-check.sh                   # everything CI checks
```

- `test/fixtures/ieb/v0.1.0/` holds 39 committed bundles (valid, tampered, malformed,
  limits). `expected.json` pins each one's exit code and problem codes.
- `test/spec/build_from_spec.py` is a producer written from the spec alone, with no Lapilli
  code.
- Crafting new inputs: `crates/lapilli-bundle/examples/gen_fixtures.rs` shows how each
  malformed case is built (raw tar headers written by hand).

## Known and accepted

These are documented in the design logs and are not findings unless you show they are
worse than stated:

- Signing time is self-asserted.
- Without `--key`, a signature proves nothing about the signer (`unpinned`).
- The verifier trusts the zstd crate's decoder and the `tar` crate's raw header parsing.
- The `.ieb` is decompressed in a single pass; memory is bounded by the per-file cap and the
  zstd window, not by the total size.

## What to report

For each finding:

- the input, as a file or a script that builds it;
- the command and its actual versus expected verdict;
- which invariant above it breaks.

Security issues go through GitHub private vulnerability reporting (`SECURITY.md`); anything
else through an issue. The maintainer records the review (who, when, the commit reviewed,
the findings and their dispositions) in `docs/independent-review-log.md`. That record is
the checklist item in `RELEASE.md`.
