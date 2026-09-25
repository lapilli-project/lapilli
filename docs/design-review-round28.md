# Design review — round 28 (the sync audit: do the documents still describe the code?)

Constitution: **v0.7.1** (this round only synced it; no rule changed). Target: not one design
but the *record* — `README.md`, `DESIGN.md`, every `docs/design-*.md` status line, the two
specs, `CHANGELOG.md`, `docs/COMPATIBILITY.md`, the chart's `NOTES.txt`, the CI workflows, and
the vault's constitution, skill and memory — against the tree at `8a92aae` (2026-09-25).

## 0. Gates

- **Category:** public-facing documents and the release process — yes.
- **Break-even:** the downside is concrete. The next leap is the first public release
  (`ROADMAP.md` §2); a reader's first hour is the docs, and after 27 rounds each document had
  been patched at a different time. Four read-only auditors in parallel, one arbiter pass, then
  five editing passes on disjoint file sets: cheaper than one adopter reading a false claim.
- **Snapshot:** `8a92aae`. **Roles:** author = Claude (all of it), arbiter = Claude, owner =
  the user. The independence caveat of every earlier round holds — this one is *calibration*,
  and it found what a calibration pass can find: internal contradiction, not unknown unknowns.
- **Not a strategy round.** The identity sentence (`DESIGN.md`) was the check, not a subject;
  no finding below touches it.

## 1. The finding that mattered

**GitHub CI has been red on every push since 2026-09-17, and the local release gate could not
see it.** Two causes, both outside the code:

1. `.gitignore` has `*.ieb`. Of the 43 frozen compatibility fixtures only the four added in
   round 25 had been force-added; the other 39 existed on one laptop. `scripts/release-check.sh`
   *regenerates* the fixtures before it tests them, so the local gate manufactured the input the
   remote gate lacked, and `cargo test --workspace` on the runner failed
   `every_released_fixture_keeps_its_verdict` on 39 cases, every time. `COMPATIBILITY.md`
   said "golden + negative fixtures checked in CI"; the spec said "test vectors for every rule
   are in `test/fixtures/ieb/`". Both were true of the laptop.
2. quay.io now requires a login for `minio/*` — not only for digest requests, as round 26
   concluded from a kept cluster, but for the repository itself (`/api/v1/repository/minio/minio`
   answers 401). Round 26's fix staged the images from the host's Docker cache, which a GitHub
   runner does not have, so the export suite fails there before it starts.

The method lesson is the same shape as round 21's *"generation and verification that share a
blind spot agree with each other"*: **a local gate that produces its own inputs cannot notice
what the remote gate is missing.** Twenty-seven rounds of "gate green" were statements about one
machine. Nothing in the loop asked the remote gate; `gh run list` is one command.

**Applied.** The fixture generator was checked to be deterministic (two runs identical, and
identical to the tree), so the 43 files are now tracked (`!test/fixtures/ieb/**/*.ieb`). The
export suite gained an `E2E_IMAGE_MIRROR` fallback with the same digest check as the cache
path, and `scripts/mirror-e2e-images.sh` fills a mirror with crane/skopeo so the digest is
preserved. **Filling it creates a package on the org and is the owner's call**; until then the
CI export suite fails with the reason printed, which is the honest state.

## 2. What the four auditors found (headline rows only)

| Area | WRONG / CONTRACT-BREAK | STALE | Applied by |
|---|---|---|---|
| `README.md` | round count (19 → 27), 29 → 41 series, 21 → 26 alert rules, "v0.2: consumer adapters" as the roadmap | no bullets for postmortem, mcp, the perishable profile, storm bounds, `--output json`, permissions-follow-profiles | editing pass 1 |
| `DESIGN.md` | cosign v2 `verify-blob` as the conformance anchor (§4, §8) where §8 item 6 already said openssl; KMS "v0.2" in the §5 table, the diagram and §7; "v0.1 bundles had no redaction"; §11 "remaining: postmortem draft only" | §8 written as current scope; the diagram missing metrics, permissions, retirement and the readers; `CaptureProfile` field list; the Secret-tracking sentence | arbiter (§11 re-baselined to a **v0.1.0 row**: there was never a v0.1/v0.2 release split) |
| design docs | `pods/log` false green "exists today" (fixed a1e9485); three metric/alert names never shipped (`lapilli_captures{retired}`, `_unretirable`, `LapilliCapturesAccumulating`); `lapilli pull` (never existed); `helm-renders.sh` "validates strict" (it does not, `run.sh` does); `design-kms.md` key archive "planned" (shipped) and a `RELEASE.md` item that did not exist; `metrics.md` runbook cause that the code makes impossible; `$labels.check` on a rule with no such label | six status lines (`permissions-by-profile` "implementing", `trigger-and-load` "proposal", `export`/`remote-verify` "v1", `COMPATIBILITY` "v2", `independent-review` "rounds 1–19"); the 6.2 KB figure told three ways | editing passes 4 and 5 |
| spec / CHANGELOG / chart / CI | the untracked fixtures (above); CHANGELOG missing the logs-403 verdict change, `maxCapturesPerPayload`, the rename migration; `NOTES.txt` printing "80% of the volume" when `maxBytes` is `0`; CI lacking two `release-check.sh` steps (fixture reproduction, promtool) | `resources/statefulset.json`/`daemonset.json` missing from the spec layout; "39 fixtures" in four places; `test/mcp/check.sh` orphaned from every gate | editing pass 3 |
| vault | skill still said "필수 렌즈 둘" and lacked v0.5.0's measure-before-folding and bounded-category rules; Constitution's §VI fork trigger had no hook for Principle 9; Revisions 0.7.0 under-described itself | memory: eight superseded "in progress" statements; no review index | arbiter (Constitution **v0.7.1**, skill, `17 Reviews/Review 인덱스`, memory split into *status* and *history*) |

Clean axes, stated as probes: every relative link in README/DESIGN resolves; every `ieb/v1`
manifest field and rule-6 condition matches `manifest.rs`/`verify.rs`; every `verify-result/v1`
member is emitted and documented; the 13 problem codes match `ProblemCode::as_str`; all 37
`lapilli_*` series are documented and emitted; every chart value is in the schema and every env
the chart sets is read.

## 3. Judge — what was refuted or left

- **"The verify line prints `format ieb/v1`"** (README audit) — refuted by the editing pass:
  `verify_cmd.rs` prints the last path segment, `v1`. The README now matches the code; whether
  the code should print `ieb/v1` is a NIT for a later ship, not a doc fix.
- **The 6.2 KB figure.** Three documents disagreed ("no p50 exists" vs "n=201, p50 6.2 KB").
  Both were half right: it is the median of 201 *demo* bundles, which makes it a floor for real
  workloads, not their median. One sentence, three places.
- **VALID-OUT-OF-SCOPE, logged:** three spec/verifier corner cases the spec auditor found
  (`unreadable` vs `signature` code for a bad `--key` in the *library* path; a mid-stream limit
  dropping structure problems already found; unknown `alg` skipping the `cosign.pub` id check).
  None changes a verdict a released fixture pins; each is a one-line spec clarification or a
  small code change and goes to the pre-tag list, not this round.
- **Not applied on purpose:** `LAPILLI_IMAGE_DIGEST` is never set by the chart, so every chart
  install seals `producer.image_digest = "unknown"`. True, and a real gap in the layout's
  promise — but the fix is a downward-API/pod-status design question (the digest is not known
  to the container), not a doc edit. Carried to `ROADMAP.md` §4.

## 4. Verdict

**Applied; not dry in the constitution's sense and not claimed to be.** The audit was a
calibration pass by the author's own model family, and the record now agrees with the tree at
one commit. What it cannot say is that the tree is right. Two things follow from it:

1. `ROADMAP.md` exists and is the one place for order and priority; `README.md` and
   `DESIGN.md` §11 point at it. Item 0 there is the CI.
2. **The remote gate is now part of the definition of "green."** A ship is not verified until
   `gh run list` shows the push green, or the failure is explained in the round log.

## 5. Carried forward

- Fill the E2E image mirror (owner decision) and watch the first CI run that reaches the export
  suite on a runner.
- The three spec corner cases above; `LAPILLI_IMAGE_DIGEST`; `test/mcp/check.sh` into a
  gate guarded by `command -v npx`.
- Everything in `ROADMAP.md` §3 from item 3 down is the owner's.
