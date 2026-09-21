# Design Review — Round 5 (compatibility policy and the `ieb/v1` freeze)

Constitution: **Loop Engineering Constitution v0.5.0**. Artifact:
[`COMPATIBILITY.md`](COMPATIBILITY.md) and, once R1 showed the promises had no mechanism,
the `ieb/v1` verification contract in [`../spec/IEB-SPEC.md`](../spec/IEB-SPEC.md) together
with its implementation. The loop ran on the policy, then on the code that has to back it,
because a released bundle format can't be changed after the fact.

## Round 0 — scope and gates

- **Target:** a compatibility policy for the first public release, drafted as snapshot v0.
- **Category:** a public, hard-to-reverse promise, security-relevant (a verifier of
  untrusted input).
- **Break-even:** cost ≈ 11 critic runs plus arbiter time. Downside: an unkeepable or
  missing promise published to the first followers, and a bundle format frozen with holes.
  Downside ≫ cost.
- **Sizing:** high (public + irreversible). Security and legal lenses were mandatory.
- **Independence: not achieved (calibration only).** All critics were Claude. As in round
  4, the load-bearing facts were measured instead: the real MSRV (1.89, not the declared
  1.79), and scale-event wording on a live Kubernetes 1.30 cluster.

**Found before the loop started, by measurement:** Kubernetes 1.30 words scale events
"to 1 from 0"; 1.37 says "from 0 to 1". The parser knew only the newer order, so on 1.30
every rollback timing silently degraded to `unknown`. Fixed in `5a30b79`. The E2E had only
ever run on one Kubernetes version.

| Round | Snapshot | Critics | Result |
|---|---|---|---|
| R1 diverge | policy v0 `48847c1dd2a1` | format longevity · Kubernetes practice · security · legal · adopter/maintainer | 2 BLOCKER (in the code, not the prose), ~15 MAJOR |
| R2 converge | policy v1 `c96147174a29` | rotated: verification contract · wording + feasibility | 1 NEW-BLOCKER, most folds WEAK |
| R3 confirm | the implemented v1 (code, fixtures, spec) | one critic with a live attack harness | 1 BLOCKER, 4 MAJOR |
| R4 confirm | R3 fixes | one critic re-attacking them | 1 BLOCKER, 3 MAJOR, 1 MINOR (tar extension records) |

## R1 — the policy promised things the verifier could not do

The prose was not the worst problem. The code under it was:
- **`lapilli verify` never read `schema_version`.** "Refuse an unknown major" had no
  mechanism behind it, and one exit code (1) meant both "tampered" and "upgrade lapilli".
- **PARTIAL compared list lengths:** `run=[logs, logs]` satisfied `intended=[logs, metrics]`.
- **The signature could be stripped:** `signature/` sits outside the hash tree and nothing
  declared that a signature should exist.
- **The temp dir name was guessable** and created with `create_dir_all`, so a pre-created
  attacker directory was reused. There was also no decompression limit.
- **The hash-root encoding and path rules were unwritten**, so no second implementation
  could exist.

The three that were plain bugs (temp dir, limits, PARTIAL) were fixed at once
(`53c6613`). The rest became the v1 contract.

Policy findings, all APPLY:
- **"forever" / "disputes" read like a warranty and an evidence claim** (legal). Reworded:
  reading is kept, vouching follows current security knowledge; Apache-2.0 §7–8 note; no
  claim about fidelity or evidentiary value.
- **The "Enforced by" column cited CI jobs and files that did not exist.** Three lenses
  found this independently, and the arbiter had written it. Now a "Gate before v0.1.0"
  column: the release isn't cut until each item exists.
- **Helm doesn't upgrade `crds/`.** CRDs are now additive-only within a served version, and
  the upgrade step is documented.
- **"3 newest minors" was too narrow, and a CI matrix too expensive** for one maintainer.
  These conflicted, and were merged by cadence: 1.37 E2E on every change; 1.30 and 1.37
  before each tag; recorded-event unit tests for version differences. Claimed as
  "tested 1.30 and 1.37, expected to work 1.31–1.36".
- `--output json` was dropped from v0.1 (promised stable before it existed). Prebuilt CLI
  binaries were added, since an auditor needs a downloadable verifier, not an MSRV.
  SECURITY.md is the single source for supported versions. GOVERNANCE lists the policy as a
  significant change.

## R2 — the new contract, attacked before coding it

- **The signing declaration doesn't stop an attacker.** Without `--key`, anyone can re-seal
  a bundle with `signing: null`. The docs now say exactly that: the declaration catches
  accidental loss, and authenticity comes only from `--key`. `--require-signature` was
  dropped (meaningless without a key). `key_id` was added so a wrong key is reported
  clearly.
- **Exit 3 was a way to dodge `== 1` scripts.** Once a bundle is recognised as v1,
  anything wrong with it is 1. Exit 3 is kept for an unknown major, unreadable input, and
  the limits. Usage errors moved from clap's 2 (which reads as PARTIAL) to 64. The docs
  give a `case` snippet: only 0 passes.
- **`features` protected nothing an attacker couldn't re-seal, and duplicated
  `collectors_run`.** Replaced by a frozen table of required files per collector.
- **Filesystem hazards** (case-insensitive and NFD filesystems, lossy names, duplicate tar
  entries). Fixed by never extracting: the `.ieb` is hashed while streaming, and path rules
  are `[A-Za-z0-9._-]` segments, unique case-insensitively.
- **NEW-BLOCKER (feasibility): only 3 of 10 "enforced today" cells were true.** Handled by
  the gate column above.

## R3 — the implementation, attacked with a harness before the freeze

The critic built hostile bundles and ran them against the binary:

1. **BLOCKER: two `manifest.json` entries (an evil one first) verified OK.** `manifest.json`
   and `signature/*` bypassed the duplicate and case checks, and a streaming consumer
   reading the first entry saw a fake incident. Fixed: duplicates and case collisions are
   checked over every path, reserved names in any case variant are FAILED.
2. **MAJOR: `key_id` was ambiguous.** A compressed-point key hashed differently. Now defined
   over the uncompressed SPKI DER, with a fixture that verifies with a compressed key file.
3. **MAJOR: duplicate JSON member names were last-wins.** Signed bytes could show one hash
   and enforce another. Now FAILED (strict pre-parse).
4. **MAJOR: the closed `signature/` set would have forced v2 for the roadmap's sigstore
   bundle and RFC 3161 token.** `signature/ext/` is reserved and ignored by v1.
5. **MAJOR: the controller could seal bundles its own verifier rejects** (a profile listing
   a collector twice). Fixed: dedup in the collector, and `x-kubernetes-list-type: set` on the
   CRD.

Also fixed: limits apply in directory mode too (both forms get the same verdict); an exact
`schema_version` grammar; pax and GNU long-name records allowed (reversed in R4: they
turned out to be the attack surface); entries hashed while streaming. Every attack became a
fixture, checked by `cargo test`.

**Independent producer.** `test/spec/build_from_spec.py` builds a bundle from the
normative spec section alone (Python standard library, no Lapilli code), including an
unknown future field, and `lapilli verify` accepts it in CI. This is the closest thing to an
independent implementation available now.

## R4 — confirmation (the last round the budget allows)

The critic re-attacked every R3 fix with 17 new hostile bundles. The R3 fixes held: duplicate
and `./` manifests, case variants, escaped duplicate JSON keys, `signature/ext/../`,
compressed keys, collector dedup, and the schema grammar. What it found instead came from
**tar extension records**:

1. **BLOCKER: a pax `size=` override** delivered a 100 MiB `manifest.json` behind a ustar
   header claiming 0 bytes. It passed both the limits and the small-file cap and verified
   OK.
2. **MAJOR: a GNU long-name record** made the tar crate buffer 1.99 GB before Lapilli saw the
   entry.
3. **MAJOR: `logs` and `logs/index.json` in one bundle** verified OK as a `.ieb` but can't
   be unpacked, so the two forms disagreed.
4. **MAJOR: the 16 MiB in-memory cap** wasn't in the spec and applied only to archives.
5. **MINOR: a pax `path=` override** differing from the ustar name was accepted.

Findings 1, 2 and 5 share a root cause: interpreting extension records is itself the
attack surface. So they were fixed **by design, not by patching**:
- `ieb/v1` is plain ustar. Pax and GNU records are FAILED, and are read in raw mode so
  their payload is never buffered.
- Paths must fit the ustar prefix+name fields.
- The producer writes ustar headers itself; overlong Kubernetes names in paths are hashed.

Finding 3 became a "never both a file and a directory" rule, covering directory entries as
well (the arbiter found `manifest.json/` + `manifest.json` still passing after the first
fix, and closed it). Finding 4 is now in the spec, applies to both forms, and is exit 3.

All of the critic's attack bundles (R3 and R4, about 50) were re-run against the fixed
binary. Every hostile one is rejected, peak memory is 4–12 MB (1.99 GB before), and the
remaining OKs are benign: data after the end-of-archive marker, which every reader ignores,
and a contiguous-file entry. 39 fixtures now cover these cases.

## Verdict

**Time-boxed, not dry.** R4 was the last round the high tier allows, and its fixes were
folded without a further critic round. They are backed by the critic's own reproductions
and fixtures instead. The trend was BLOCKER-heavy in every round (R1 in the code, R2 in
feasibility, R3 in duplicate handling, R4 in tar extensions), each time one layer deeper
into the container format. That is the signature of an attack surface, not of churn.

**Recommendation before tagging:** have a human, or a non-Claude reviewer, attack
`read_ieb`, `Contents` and the manifest parse. Independence was not achieved in this loop,
and the verifier of untrusted input is exactly where correlated blind spots would hurt. That
review is a gate item in `RELEASE.md`.

## Carried into the release checklist

`RELEASE.md`. The gate items are: the release-gate workflow on 1.30 and 1.37, the MSRV job,
the fixtures, the spec-only producer, `values.schema.json`, `CHANGELOG.md`, SECURITY.md,
private vulnerability reporting enabled, and public packages.
