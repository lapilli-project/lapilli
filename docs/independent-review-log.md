# Independent review log: the bundle verifier

The record `RELEASE.md` asks for: who reviewed the verifier of untrusted input, when, at
which commit, what they found, and what was done about it. The brief is
[`independent-review.md`](independent-review.md).

## 2026-09-25, model review (non-Claude, API, no human)

| | |
|---|---|
| Snapshot | `b9973f5` (clean tree; `cargo test --workspace` at this commit: 238 passed, 0 failed) |
| Reviewer that ran | OpenAI `gpt-5-2025-08-07` (Responses API, reasoning effort `medium`, one call: 60,351 input tokens, 23,440 output of which 19,968 reasoning, `status: completed`) |
| Reviewer that did not run | Google Gemini. No usable review was obtained; see below. |
| Triage | Claude (the maintainer's assistant), by reading the code and by building inputs with raw ustar headers under a scratch directory and running the `lapilli` binary built from this commit. Not independent; recorded so the reader can tell the two apart. |
| Human review | none yet |

### What the reviewers saw

One packet, 224 KB, the same for both providers:

1. `docs/independent-review.md` in full.
2. `spec/IEB-SPEC.md`, the normative section only (rules 1 to 10, lines 237 to 383).
3. Full, line-numbered source of `crates/lapilli-bundle/src/{verify,hashtree,manifest,sign,pack}.rs`
   and `crates/lapilli-cli/src/{verify_cmd,remote,mcp}.rs`.

Nothing was cut: the eight files total 203 KB, under the 250 KB cap. Test modules and the
parts of `remote.rs` and `mcp.rs` outside the brief's scope (URL parsing, S3 version listing,
the HTTP transport) were left in as context and are not part of the scope.

System prompt (both providers): find concrete, reproducible defects on the axes the brief
names (resource exhaustion beyond the limits, path escapes, verdict flips, panics, integer
overflow, TOCTOU in MCP file serving, spec-to-code gaps); per finding give severity, function
and line, triggering input, expected versus actual, a one-line fix; no style; name the probes
tried on clean axes; at most ten findings; "assume the authors are competent and looking for
what they missed; praise is not a finding."

### Gemini: attempts and why there is no result

Eight calls to `generativelanguage.googleapis.com/v1beta/models/<model>:generateContent`
with the same body. Every raw error response is kept in the scratch directory.

| Model | Result |
|---|---|
| `gemini-2.5-pro` (the model the plan named) | 404: "no longer available to new users" |
| `gemini-3.1-pro-preview` (the 404's recommendation) | 429: free-tier quota for this model is 0 |
| `gemini-2.5-flash` | 404, same message |
| `gemini-3.8-flash` (the 404's recommendation) | 503 "high demand"; then one 200 that stopped at `MAX_TOKENS` after 654 visible tokens (32,215 thinking tokens): the visible text is a fragment of reasoning about `parse_versions` and holds no finding; then 503 five more times over eight minutes of backoff |
| `gemini-3.7-flash`, `gemini-flash-latest` | 503 |

The fragment's one half-formed thought (S3 reports `VersionId` as the string `null` when
versioning is suspended) was not developed into a claim and is not counted. A second
non-Claude reviewer is still owed; see "Limits".

### GPT-5 findings and their disposition

Ten findings, in the reviewer's order and with its severities. Line numbers are in the file
named. "Probe" is what was run at this commit; the inputs were built with raw ustar headers
and `zstd -3`, then `lapilli verify` (and `lapilli unpack` for the unpacked form).

| # | Reviewer's claim | Sev. (theirs) | Disposition | Evidence |
|---|---|---|---|---|
| 1 | Directory mode reads every file fully into memory (`verify.rs` `walk`, line 583 `std::fs::read`); the 16 MiB in-memory cap applies only to the small files | BLOCKER | **CONFIRMED** as a fact; severity disputed, see below | A bundle with one 256 MiB `logs/big.bin`: peak RSS 262.7 MiB verifying the directory, 9.3 MiB verifying the same bundle as `.ieb`. Same verdict (OK) both ways. |
| 2 | Directory entries are not counted toward the 100k entry limit (`read_ieb_from` lines 460 to 467 `continue` before `over_limits`) | BLOCKER | **CONFIRMED** | 120,000 `dN/` directory entries plus the three files: exit 0, OK. Control, 120,000 file entries: exit 3, "exceeds the verifier limits". |
| 3 | `input.sha256` covers trailing bytes after the end-of-archive that the verifier never read (`verify_local`, `body.drain()` line 400), breaking "what is hashed is what was verified" | MAJOR | **REFUTED** as stated; a spec question remains | `ok-unsigned.ieb` + 4 KiB of zeros: verdict OK, `input.sha256` is the digest of the whole file, `input.size` 4807. That is the documented intent of `drain` ("so the hash covers the whole object", `remote.rs` line 213): the digest identifies the object a store or `--expect-sha256` names, and the proposed fix (skip the drain) would make it disagree with `sha256sum`. `--expect-sha256 <digest of the original>` on the padded file: exit 1, `digest`. What the probe does show: bytes after the tar end-of-archive marker do not affect the verdict, and rule 9 says nothing about trailing data. That is a decision for the spec, not a code defect. |
| 4 | `ustar_representable` accepts 256 bytes; the spec says 255; packed and unpacked could diverge | MAJOR | **REFUTED**; documentation nit | A 256-byte path (`prefix` 155 + `/` + `name` 100) is exactly what a ustar header can hold, and both forms verified OK; a 257-byte path is refused in both. The code is right; the spec's "≤ 255 bytes" (rule 1) and the error text in `check_path` (`hashtree.rs` line 111) are off by one and should say 256. |
| 5 | An empty **directory** entry `Signature/` is not refused (`add_dir` has no reserved-name check; `add_hashed` has one at lines 369 to 378) | MAJOR | **CONFIRMED**, spec gap; no integrity effect | `Signature/` (type `5`, no files under it): exit 0 packed and unpacked. With a file inside, `Signature/x`: exit 1, "reserved name used with different case or type", as designed. Rule 1 says a first segment `Signature/` in any case makes the bundle FAILED; the code applies that to files only. |
| 6 | pax/GNU extension records are not counted and each pushes a problem string; the loop continues (`read_ieb_from` lines 443 to 451) | MAJOR | **CONFIRMED** | 120,000 pax (`x`) records plus the three files: exit 1 (FAILED, correct) with 120,001 problem lines on stderr and 36 MiB RSS; the entry limit never applied. The same holds for every entry the loop `continue`s past before line 475: link and special entries (line 469), non-UTF-8 names (line 454). The byte limit is on **compressed** input (`Body`, 2 GiB), and identical 512-byte headers compress by three orders of magnitude, so the number of such records one `.ieb` can carry is effectively unbounded; memory and time grow with it. |
| 7 | With an unknown `signing.alg` and no `--key`, a `cosign.pub` whose id differs from `signing.key_id` is not reported (arm at line 1073) | MAJOR | **PLAUSIBLE**; spec ambiguity, no verdict flip | `alg: unknown-x`, `manifest.sig` present, `cosign.pub` = `other.pub`, no `--key`: exit 0, `signed:unpinned`, no problem. With `--key`: exit 1. Rule 8 says both "its id MUST equal `signing.key_id`" and "an unknown `alg` means authenticity is not established (FAILED only when a caller key was supplied)"; the code follows the second sentence. Either the spec should say the self-consistency check is alg-independent, or the code should still report the mismatch as a notice. Nothing an attacker gains: unpinned is what an unknown alg reads as with or without the check. |
| 8 | `unpack` extracts unknown files under `signature/` (`pack.rs` line 112 to 115 skip `check_path` for `signature/*`) | MINOR | **CONFIRMED**, no consequence | `signature/evil.dat`: unpack succeeds and writes it under `dest/signature/`; verify then gives exit 1 ("unexpected: signature/evil.dat") for both the `.ieb` and the unpacked directory. Same verdict, no escape. `unpack` applies the container rules, not the tree rules; the brief's "same rules" was read as the latter. |
| 9 | Directory entries that differ only by case (`Logs/`, `logs/`) are not detected (`add_dir`) | MINOR | **CONFIRMED**, spec gap; no integrity effect | Both entries as type `5`, mode 0755: exit 0 packed and unpacked (on APFS, case-insensitive, they are one directory). No file path collides, so nothing in the tree is affected. |
| 10 | Text output spells `CANNOT-EVALUATE`, JSON `CANNOT_EVALUATE` | MINOR | **CONFIRMED**, cosmetic | `cannot-v0.ieb`: text `CANNOT-EVALUATE`, JSON `"CANNOT_EVALUATE"`. Text output is documented as not stable (`verify_cmd.rs` lines 1 to 3). |

Counts: 7 confirmed (of which 4 have no effect on any verdict or on integrity), 1 plausible,
2 refuted.

On severity. The reviewer called 1 and 2 BLOCKER. 1 is a real difference between the two
input modes in what memory is bounded by (the largest file in a directory, up to the 2 GiB
total, versus the 16 MiB cap plus the zstd window for a stream), but the directory form is a
bundle the operator has already unpacked onto their own disk, and the verdict is the same;
MAJOR at most. 2 and 6 are the same defect, "not every entry is counted", and together they
are the one resource-limit gap in this review that an attacker-controlled `.ieb` reaches: the
entry limit is applied only to regular files. That one is MAJOR and should be fixed before
release; the fix is a single `over_limits(0)` at the top of the loop body, before any
`continue`.

### Found during triage (not from the models)

While reproducing 9, an unrelated packed-versus-unpacked divergence showed up, attributed to
the triage, not to the review:

- **Entry modes survive `unpack` and change the verdict of the unpacked form.** `unpack`
  applies the tar header's mode (`entry.unpack`, `pack.rs` line 134). A directory entry with
  mode 0644, or a regular file entry with mode 0000, unpacks without complaint; `verify` of the
  `.ieb` is OK (streaming ignores modes), `verify` of the unpacked directory is exit 3,
  "Permission denied". Same bytes, different verdict, which the brief lists as an attacker
  win, though the direction here is a denial (3), not a false OK. It also reaches `lapilli
  mcp`: `read_file` and `summary` stage the `.ieb` with `unpack` after `verify` said OK, and
  a 0000-mode file then fails to read; a 0644 directory inside the `TempDir` may also survive
  the drop. Fix: have `unpack` set modes itself (0644 files, 0755 directories) instead of
  honouring the header's, or `chmod` after extraction. Severity MINOR to MAJOR depending on
  how much the MCP path is relied on.

### Clean axes the reviewer claimed

GPT-5 listed what it probed and found clean: entry type checked before size and pax/GNU
records refused (`raw(true)`); duplicate and case-colliding files caught at collection time
and in the manifest listing, including a second `manifest.json`; `unpack` refusing absolute
paths, `..` and links, and the MCP `resolve`/`resolve_bundle` sandbox (canonicalize, then a
prefix check); `Body` mapping the byte limit to `limit` and read errors to `unreadable`
rather than a verdict; ECDSA P-256 DER with low-S normalisation, `key_id` as SHA-256 of the
SPKI DER; coverage set semantics including `deferred`; verdict precedence FAILED over
PARTIAL and cannot-evaluate; hash-tree root and the modified/missing/unexpected cases. It
reported no panics, no integer overflow and no TOCTOU finding on the MCP path. Its
"tested with" phrasing describes reasoning over the packet, not execution: the model had no
tool access.

### Limits of this pass

- A model review is not a human review. One model read the code once, with no ability to
  run it; what it could not see it could not report, and the clean-axis list is its claim,
  not a demonstration.
- Only one of the two intended reviewers produced a result, so the "two independent readers"
  intent of the brief is not met. The two providers would also have seen the identical
  packet and prompt, so even a complete pair would share the packet's framing.
- The packet was the source at this commit plus the normative spec; no fixtures, no
  `expected.json`, no design logs. Findings 3, 4, 7 and 8 are readings of the spec text as
  much as of the code, and two of them were refuted by checking what ustar and `drain`
  actually do.
- No fuzzing in this pass. The probes were one crafted input per claim, at a scale (120k
  entries, 256 MiB) chosen to show the limit is or is not applied, not to find the point of
  failure.
- The triage was done by Claude, which shares the blind spots this brief exists to get
  around. The one extra finding it added is therefore not independent either.

### Dispositions still open

Fixes for 2/6 (count every entry), 5 and 9 (reserved and case rules for directory entries),
the mode issue found in triage, the spec's off-by-one in rule 1, and the rule 8 wording are
for the maintainer to decide and record here. A second non-Claude reviewer, or a human, is
still required by `RELEASE.md`.

Raw material (packet, prompt, every provider response including the failed calls, the probe
builder and the crafted inputs) is in the session's scratch directory, not in the repository.

## Disposition (2026-09-25)

Fixed in the working tree on top of `7d59cf5` (not yet committed when this was written). Every
pinned verdict and code set in `test/fixtures/ieb/v0.1.0/expected.json` is unchanged, and
`gen_fixtures` reproduces all 43 released `.ieb` files byte for byte (`diff` of `expected.json`
and `cmp` of each file, both clean). `cargo fmt --check`, `cargo clippy --workspace
--all-targets -D warnings` and `cargo test --workspace` (246 tests) pass.

| # | Finding | Disposition | Where | Test |
|---|---|---|---|---|
| 1 | Directory mode reads whole files | **Fixed** | `verify::walk` now hashes through `hashtree::sha256_file` (64 KiB `BufReader`, read bounded by the size already counted against the limits); `hashtree::collect` (sealing) streams the same way | `verify::review_tests::directory_mode_hashes_large_files_without_holding_them` (64 MiB file in a tempdir: OK, then `modified:` after one appended byte) |
| 2, 6 | Not every entry counted | **Fixed** | `verify::read_ieb_from`: `over_limits` runs first for every entry, whatever its type; only regular files add bytes (type before size, as before); over the limit is `limit` / exit 3 | `verify::review_tests::every_entry_counts_toward_the_entry_limit` (100,001 directory entries and 100,001 pax records → limit; 100 of each → not limit) |
| 3 | Trailing bytes in `input.sha256` | Refuted; **spec sentence added** | `spec/IEB-SPEC.md` rule 1: bytes after the end-of-archive marker do not affect the verdict and are part of `input.sha256`, which names the object | none (behaviour unchanged; `f3-junk.ieb` still OK) |
| 4 | 255 vs 256 | Refuted; **wording fixed** | rule 1 path sentence and the `hashtree::check_path` error text now say 256 | none (behaviour unchanged; `ok-long-path.ieb` and `f4-256.ieb` still OK) |
| 5 | `Signature/` as a directory entry | **Fixed** | `verify::Contents::add_dir`: reserved-name rule (any case of `Signature` as first segment; a directory named `manifest.json` in any case); `signature/` itself stays legitimate | `verify::review_tests::reserved_name_directory_entries_are_refused` |
| 7 | Unknown `alg`, mismatched `cosign.pub`, no `--key` | **Spec clarified; a notice, not a check** | rule 8: the self-consistency check is defined only for a known `alg`, since `key_id` is derived per algorithm; `verify::v1` pushes a `notice` in that arm | `verify::review_tests::unknown_alg_without_key_is_unpinned_with_a_notice` |
| 8 | `unpack` writes `signature/*` | Left as is (no consequence: both forms FAILED) | | |
| 9 | `Logs/` beside `logs/` | **Fixed** | `add_dir` (against another directory entry or the directory of a file) and the ancestor loop in `add_hashed` (a file under the case twin of an earlier directory entry) | `verify::review_tests::case_twin_directory_entries_are_refused` |
| 10 | `CANNOT-EVALUATE` vs `CANNOT_EVALUATE` | Left open (text output is documented as not stable) | | |
| triage | `unpack` honours header modes | **Fixed** | `pack::unpack`: directories 0755, files 0644 whatever the header says (`set_mode`); fifo, device and sparse entries are refused like links | `pack::tests::unpack_ignores_header_modes` (dir 0644 + file 0000: `.ieb` OK, unpacked directory OK, modes 0755/0644), `pack::tests::unpack_refuses_special_entries` |

Why 7 is a notice and not the check: rule 5 defines `key_id` for `ecdsa-p256-sha256` only.
Parsing `cosign.pub` as a P-256 key under an unknown `alg` and failing the bundle on a
mismatch would fail every bundle sealed by a producer newer than this verifier that ships its
key — the moving-denylist shape `docs/design-review-round24.md` warned about. The one-line
code change that would make the two sentences agree is exactly that, so the spec was made to
say what the code does, and the code now says when it skips the check.

### The crafted probes, before and after

Same inputs as the table above, `target/debug/lapilli verify` at this tree.

| Probe | Before | After |
|---|---|---|
| `f1.ieb` / `f1-dir` (one 256 MiB file) | OK, 9.3 MiB / OK, 262.7 MiB peak RSS | OK, 4.8 MiB / OK, **2.5 MiB** |
| `f2-dirs.ieb` (120,000 directory entries) | OK, exit 0 | **CANNOT-EVALUATE, exit 3**, `limit` |
| `f2-files.ieb` (120,000 files, control) | exit 3 | exit 3 |
| `f3-junk.ieb` (4 KiB after the archive) | OK | OK (spec now says so) |
| `f4-256.ieb` (256-byte path) | OK | OK |
| `f5.ieb` (`Signature/` directory entry) | OK, exit 0 | **FAILED, exit 1**, "reserved name used with different case or type: Signature/" |
| `f5b.ieb` (`Signature/x` file) | FAILED | FAILED (now two problems: the directory and the file) |
| `f6-pax.ieb` (120,000 pax records) | FAILED, 120,001 problem lines, 36 MiB | **CANNOT-EVALUATE, exit 3**, 1 problem line, 13 MiB |
| `f7.ieb` (unknown `alg`, other `cosign.pub`, no `--key`) | OK unpinned, no problem | OK unpinned, **one `notice`** |
| `f8.ieb` (`signature/evil.dat`) | FAILED | FAILED |
| `f9.ieb` / `f9b.ieb` (`Logs/` + `logs/` directory entries) | OK, exit 0 | **FAILED, exit 1**, "paths differ only by case" |
| `n1-dirmode.ieb` (directory entry mode 0644) | `.ieb` OK; unpacked directory exit 3 "Permission denied" | `.ieb` OK; **unpacked directory OK**, `logs` is 0755 |
| `n2-filemode.ieb` (file mode 0000) | `.ieb` OK; unpacked directory exit 3 | `.ieb` OK; **unpacked directory OK**, file is 0644 |

### Still open after this pass

- Directory mode counts only files toward the entry limit (`verify::walk`), as it always
  did: a `.ieb` of 100,001 directory entries is `limit` while the same bundle unpacked
  verifies. The direction is a denial, and an empty directory on disk is not the attacker's
  512-byte header; left as is and noted.
- `docs/COMPATIBILITY.md` line 83 still says "≤ 255 bytes"; it was outside the files this
  fix was allowed to touch.
- A PromQL result file over 64 KiB is now judged the same way in both modes (counted as
  holding data, not parsed); before this, directory mode parsed it whatever its size. No
  verdict depends on it.
- A second non-Claude reviewer, or a human, is still required by `RELEASE.md`.

## Fuzzing, same day

Three libFuzzer targets (`crates/lapilli-bundle/fuzz/`, seeded with the 43 fixtures) ran
8 minutes each on the tree **before** the fixes above: `verify_tar` 432,784 executions,
`verify_reader` 364,687, `manifest_json` 14,815,480 — no crash, timeout or out-of-memory
(`-rss_limit_mb=2048 -timeout=10`). That bounds the panics a fuzzer could reach in that time
and says nothing about the logic gaps the model found: none of findings 1–9 is a crash, which
is why the two methods are run together, not instead of each other.
